package app

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// httpRequests joins handler completion, including upgraded connections that
// net/http Shutdown does not own. The application supplies the shutdown policy.
// Handlers retain their existing completion and billing responsibilities.
type httpRequests struct {
	cancel   context.CancelFunc
	mu       sync.Mutex
	active   int
	draining bool
	forced   bool
	done     chan struct{}
	hijacked map[net.Conn]struct{}
}

type requestConnectionKey struct{}

func newHTTPRequests(server *http.Server) *httpRequests {
	ctx, cancel := context.WithCancel(context.Background())
	r := &httpRequests{cancel: cancel, done: make(chan struct{}), hijacked: make(map[net.Conn]struct{})}
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	server.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, requestConnectionKey{}, conn)
	}
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		if state != http.StateHijacked {
			return
		}
		r.mu.Lock()
		force := r.forced
		if !force {
			r.hijacked[conn] = struct{}{}
		}
		r.mu.Unlock()
		if force {
			_ = conn.Close()
		}
	}
	next := server.Handler
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		if r.draining {
			r.mu.Unlock()
			w.Header().Set("Connection", "close")
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		r.active++
		r.mu.Unlock()
		defer func() {
			var upgraded net.Conn
			r.mu.Lock()
			if conn, ok := req.Context().Value(requestConnectionKey{}).(net.Conn); ok {
				if _, owns := r.hijacked[conn]; owns {
					upgraded = conn
				}
				delete(r.hijacked, conn)
			}
			// A hijacked protocol stays owned by its handler until return, even
			// when panic bypasses its ordinary connection cleanup.
			if upgraded != nil {
				r.mu.Unlock()
				_ = upgraded.Close()
				r.mu.Lock()
			}
			r.active--
			if r.draining && r.active == 0 {
				close(r.done)
			}
			r.mu.Unlock()
		}()
		next.ServeHTTP(w, req)
	})
	return r
}

func (r *httpRequests) beginDrain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.draining {
		r.draining = true
		if r.active == 0 {
			close(r.done)
		}
	}
}

func (r *httpRequests) force() {
	r.cancel()
	r.mu.Lock()
	r.forced = true
	connections := make([]net.Conn, 0, len(r.hijacked))
	for conn := range r.hijacked {
		connections = append(connections, conn)
		delete(r.hijacked, conn)
	}
	r.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func waitStopped(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
