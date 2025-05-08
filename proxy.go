// Referenced from https://github.com/jackc/pgmock/blob/4ad1a8207f65411c75c576150c6bc0e72d38640e/pgmockproxy/proxy/proxy.go
// with some (possibly breaking) changes

package pgproxy

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgproto3"
)

var (
	// Prints each messages received from frontend and backend, encoded into JSON
	PGPROXY_VERBOSE bool
)

type Proxy struct {
	frontendAddr string
	frontend     *pgproto3.Frontend
	backend      *pgproto3.Backend
}

// Each `Proxy` handles a connection between a frontend (app) and a backend (database)
func NewProxy(frontendConn, backendConn net.Conn) *Proxy {
	proxy := &Proxy{
		frontendAddr: frontendConn.RemoteAddr().String(),
		frontend:     pgproto3.NewFrontend(backendConn, backendConn),
		backend:      pgproto3.NewBackend(frontendConn, frontendConn),
	}
	return proxy
}

func (p *Proxy) Run(cache *CacheBuffer) error {
	err := make(chan error)
	go runf(cache, p.frontendAddr, p.frontend, p.backend, err)
	go runb(cache, p.frontendAddr, p.frontend, p.backend, err)
	return <-err
}

// Runs the "frontend side" of the proxy: listens to any incoming request from frontend (app) and forwards it to backend (database)
func runf(cache *CacheBuffer, addr string, frontend *pgproto3.Frontend, backend *pgproto3.Backend, errChan chan<- error) {
	{
		startup, err := backend.ReceiveStartupMessage()
		if err != nil {
			terminate(errChan, errors.Join(errors.New("invalid message received from frontend (startup)"), err))
			return
		}
		if PGPROXY_VERBOSE {
			b, _ := json.Marshal(startup)
			slog.Info("F", "addr", addr, "data", b)
		}
		frontend.Send(startup)
		if err := frontend.Flush(); err != nil {
			terminate(errChan, errors.Join(errors.New("error when sending message to backend (startup)"), err))
			return
		}
	}
	for {
		msg, err := backend.Receive()
		if err != nil {
			// This "unexpected EOF" error is somehow always triggered for each query, I'll just ignore the message
			// Might need to take a look at my testing driver implementation (pgx & rust's sqlx)
			if errors.Is(err, io.ErrUnexpectedEOF) {
				terminate(errChan, nil)
				return
			}
			terminate(errChan, errors.Join(errors.New("invalid message received from frontend"), err))
			return
		}
		if PGPROXY_VERBOSE {
			b, _ := json.Marshal(msg)
			slog.Info("F", "addr", addr, "data", b)
		}
		if PGPROXY_ENABLE_CACHE && cache != nil && !cache.CacheFrontend(addr, msg, frontend, backend, errChan) {
			continue
		}
		frontend.Send(msg)
		if err := frontend.Flush(); err != nil {
			terminate(errChan, errors.Join(errors.New("error when sending message to backend"), err))
			return
		}
	}
}

// Runs the "backend side" of the proxy: listens to any incoming request from backend (database) and forwards it to frontend (app)
func runb(cache *CacheBuffer, addr string, frontend *pgproto3.Frontend, backend *pgproto3.Backend, errChan chan<- error) {
	for {
		msg, err := frontend.Receive()
		if err != nil {
			terminate(errChan, errors.Join(errors.New("invalid message received from backend"), err))
			return
		}
		if PGPROXY_VERBOSE {
			b, _ := json.Marshal(msg)
			slog.Info("B", "addr", addr, "data", b)
		}
		if PGPROXY_ENABLE_CACHE && cache != nil {
			cache.CacheBackend(msg, errChan)
		}
		backend.Send(msg)
		if err := backend.Flush(); err != nil {
			terminate(errChan, errors.Join(errors.New("error when sending message to frontend"), err))
			return
		}
	}
}

// Terminates current connection
func terminate(errChan chan<- error, err error) {
	select {
	case errChan <- err:
	default:
	}
}
