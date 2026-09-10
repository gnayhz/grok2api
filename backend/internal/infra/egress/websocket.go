package egress

import (
	"context"
	"net/http"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// WebSocket owns one upgraded physical exchange. The body byte count is the
// received message payload, excluding framing, TLS and HTTP handshake bytes.
// Socket close is a transport fact, never proof of Provider generation success.
// Do not expose the bare connection: reads must retain physical observations.
type WebSocket struct {
	conn     *websocket.Conn
	ctx      context.Context
	id       attemptmeta.Identity
	ledger   *physicalLedger
	stop     func() bool
	readMu   sync.Mutex
	once     sync.Once
	closeErr error
}

func newPhysicalWebSocket(ctx context.Context, conn *websocket.Conn) *WebSocket {
	w := &WebSocket{conn: conn, ctx: ctx, id: attemptmeta.FromContext(ctx)}
	if trace := physicalCallFromContext(ctx).trace; trace != nil {
		w.ledger = &trace.ledger
	}
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
	if w.ledger == nil {
		return
	}
	w.ledger.mu.Lock()
	defer w.ledger.mu.Unlock()
	if entry := w.ledger.calls[w.id.ID]; entry != nil && !entry.persisted && !entry.finalized {
		entry.fact.BodyBytes += int64(n)
		if outcome != "" && entry.fact.BodyOutcome == "pending" {
			entry.fact.BodyOutcome = outcome
		}
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
		// Close interrupts an outstanding read before taking its lock; byte and
		// error observations must finish before the fact becomes publishable.
		w.readMu.Lock()
		defer w.readMu.Unlock()
		if w.ledger == nil {
			return
		}
		w.ledger.mu.Lock()
		defer w.ledger.mu.Unlock()
		if entry := w.ledger.calls[w.id.ID]; entry != nil && !entry.persisted {
			if entry.fact.BodyOutcome == "pending" {
				entry.fact.BodyOutcome = outcome
			}
			entry.fact.At = time.Now().UTC()
			entry.fact.DurationMS = time.Since(w.id.StartedAt).Milliseconds()
			entry.finalized = true
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
		if trace := physicalCallFromContext(ctx).trace; trace != nil {
			trace.ledger.mu.Lock()
			if entry := trace.ledger.calls[attemptmeta.FromContext(ctx).ID]; entry != nil {
				entry.fact.HeaderOutcome, entry.fact.Status = "upgraded", http.StatusSwitchingProtocols
			}
			trace.ledger.mu.Unlock()
		}
	} else {
		// A non-101 handshake has an ordinary HTTP rejection body. Record that
		// response status separately from a failure to establish transport.
		observationErr := err
		if response != nil {
			observationErr = nil
		}
		recordPhysicalExchange(ctx, standard, observationErr)
		if response != nil && standard != nil {
			response.Body = standard.Body
		}
	}
	observationErr := err
	if response != nil {
		observationErr = nil
	}
	recordPhysicalCallMetric(ctx, standard, observationErr)
}
