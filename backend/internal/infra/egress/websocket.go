package egress

import (
	"context"
	"net/http"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

// WebSocket owns one upgraded physical exchange. The body byte count is the
// received message payload, excluding framing, TLS and HTTP handshake bytes.
// Socket close is a transport fact, never proof of Provider generation success.
// Do not expose the bare connection: reads must retain physical observations.
type WebSocket struct {
	conn     *websocket.Conn
	ctx      context.Context
	id       attemptmeta.Identity
	journal  physical.Journal
	stop     func() bool
	readMu   sync.Mutex
	once     sync.Once
	closeErr error
}

func newPhysicalWebSocket(ctx context.Context, conn *websocket.Conn) *WebSocket {
	w := &WebSocket{conn: conn, ctx: ctx, id: attemptmeta.FromContext(ctx), journal: physical.JournalFromContext(ctx)}
	w.stop = context.AfterFunc(ctx, func() { _ = w.close("canceled") })
	return w
}

func (w *WebSocket) Context() context.Context            { return w.ctx }
func (w *WebSocket) Attempt() attemptmeta.Identity       { return w.id }
func (w *WebSocket) SetReadLimit(n int64)                { w.conn.SetReadLimit(n) }
func (w *WebSocket) SetReadDeadline(at time.Time) error  { return w.conn.SetReadDeadline(at) }
func (w *WebSocket) SetWriteDeadline(at time.Time) error { return w.conn.SetWriteDeadline(at) }

func (w *WebSocket) ReadMessage() (int, []byte, error) {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	typ, data, err := w.conn.ReadMessage()
	outcome := ""
	if err != nil {
		outcome = "read_error"
		if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			outcome = "peer_closed"
		}
		if w.ctx.Err() != nil {
			outcome = "canceled"
		}
	}
	w.observe(len(data), outcome)
	return typ, data, err
}

func (w *WebSocket) WriteMessage(typ int, data []byte) error {
	err := w.conn.WriteMessage(typ, data)
	if err != nil {
		w.observe(0, "write_error")
	}
	return err
}
func (w *WebSocket) WriteJSON(value any) error {
	err := w.conn.WriteJSON(value)
	if err != nil {
		w.observe(0, "write_error")
	}
	return err
}

func (w *WebSocket) observe(n int, outcome string) {
	if w.journal != nil {
		w.journal.ObserveBody(w.id.ID, n, outcome)
	}
}

func (w *WebSocket) Close() error {
	w.stop()
	outcome := "closed"
	if w.ctx.Err() != nil {
		outcome = "canceled"
	}
	return w.close(outcome)
}

func (w *WebSocket) close(outcome string) error {
	w.once.Do(func() {
		w.closeErr = w.conn.Close()
		w.readMu.Lock()
		defer w.readMu.Unlock()
		if w.journal != nil {
			w.journal.FinalizeBody(w.id.ID, outcome, w.id.StartedAt)
		}
	})
	return w.closeErr
}

func recordWebSocketHandshake(ctx context.Context, endpoint string, response *fhttp.Response, err error) {
	standard := fhttpResponseAsHTTP(response)
	if response != nil {
		if response.Request != nil {
			response.Request = response.Request.WithContext(ctx)
		} else {
			response.Request, _ = fhttp.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		}
	}
	if err == nil && response != nil && response.StatusCode == http.StatusSwitchingProtocols {
		if journal := physical.JournalFromContext(ctx); journal != nil {
			journal.MarkUpgraded(attemptmeta.FromContext(ctx).ID)
		}
	} else {
		observationErr := err
		if response != nil {
			observationErr = nil
		}
		recordPhysicalExchange(ctx, standard, observationErr)
		if response != nil && standard != nil {
			response.Body = standard.Body
		}
		// A rejected handshake still owns its body bytes; wrap it so the
		// journal observes drain/close outcomes exactly like HTTP calls.
		if journal := physical.JournalFromContext(ctx); journal != nil && response != nil && response.Body != nil {
			if id := attemptmeta.FromContext(ctx); id.ID != "" {
				response.Body = &physicalBody{ReadCloser: response.Body, journal: journal, id: id.ID, ctx: ctx}
			}
		}
	}
	observationErr := err
	if response != nil {
		observationErr = nil
	}
	recordPhysicalCallMetric(ctx, standard, observationErr)
}
