package wspool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type NewConnectionFunc func(context.Context) (net.Conn, error)
type Work func(*websocket.Conn) error

type Pool struct {
	NewConnection NewConnectionFunc
	TTL           time.Duration
	Delay         time.Duration
	Logger        Logger

	init sync.Once
	reqs chan request
}

type request struct {
	ctx  context.Context
	work Work
	resp chan error
}

func (p *Pool) Do(ctx context.Context, work Work) error {
	p.init.Do(func() {
		p.reqs = make(chan request)

		go func() {
			var ws *websocket.Conn
			timer := time.NewTimer(p.TTL)

			closeWs := func() {
				if ws != nil {
					closeWebsocket(ctx, ws)
					ws = nil
				}
			}

			for {
				select {
				case req := <-p.reqs:
					// check if context is exceeded and we can skip this one
					if req.ctx.Err() != nil {
						continue
					}

					if ws == nil {
						if p.Logger != nil {
							p.Logger.Infof("Opening new connection")
						}

						ws, err := p.NewConnection(req.ctx)
						if ws != nil {
							fmt.Print("WS created")
						}
						if err != nil {
							req.resp <- fmt.Errorf("failed to open new connection: %w", err)
							continue
						}

						if p.Logger != nil {
							p.Logger.Infof("Successfully opened new connection")
						}
					} else {
						if p.Logger != nil {
							p.Logger.Infof("Reusing open connection")
						}
					}

					// the buffer is cleared after each message (according to Gorilla docs)
					// do the work!
					// err := req.work(ws)
					// req.resp <- err

					bytes, err := req.work(ws, req.command)

					req.resp <- response{response: bytes, err: err}

					// close the connection if necessary
					var nerr net.Error
					if errors.As(err, &nerr) && (!nerr.Temporary() || nerr.Timeout()) {
						// if it was a timeout error, close the connection
						if p.Logger != nil {
							p.Logger.Warnf("closing connection due to non-temporary or timeout error: %s", err.Error())
						}
						closeWs()
						continue
					}

					// reset timer since we did something
					if !timer.Stop() {
						<-timer.C
					}
					timer.Reset(p.TTL)

					// delay
					time.Sleep(p.Delay)
				case <-timer.C:
					if p.Logger != nil {
						p.Logger.Infof("Closing connection")
					}

					err := closeWebsocket(ctx, ws)
					if err != nil {
						p.Logger.Warnf("closing connection due to non-temporary or timeout error: %s", err.Error())
					}
				}
			}
		}()

		if p.Logger != nil {
			p.Logger.Infof("Started pool")
		}
	})

	req := request{
		ctx:  ctx,
		work: work,
		resp: make(chan error),
	}

	p.reqs <- req

	select {
	case err := <-req.resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}