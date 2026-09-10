package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	serverws "github.com/gorilla/websocket"
)

func webSocketLedgerContext(t *testing.T, limit int) (context.Context, *inferencedomain.AttemptBudget) {
	t.Helper()
	ctx := attemptmeta.WithRequest(context.Background(), "ws-request", 7, "rules", nil)
	ctx = attemptmeta.WithAccount(ctx, 42, "grok_web", "grok-chat-fast")
	ctx = WithPhysicalCallTrace(ctx, "grok_web", "responses")
	budget := inferencedomain.NewAttemptBudget(limit)
	t.Cleanup(budget.Close)
	return WithPhysicalCallBudget(ctx, budget), budget
}

func webSocketTestLease(t *testing.T, ctx context.Context) (*Lease, *Manager) {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(&e2eRepo{}, cipher)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	lease, err := manager.Acquire(ctx, domainegress.ScopeWeb, "ws-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	return lease, manager
}

func TestWebSocketPhysicalFactsAndCancellation(t *testing.T) {
	for _, mode := range []string{"local_close", "peer_close", "cancel", "read_error", "partial_message"} {
		t.Run(mode, func(t *testing.T) {
			ctx, budget := webSocketLedgerContext(t, 1)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			lease, _ := webSocketTestLease(t, ctx)
			var handshakes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&serverws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				handshakes.Add(1)
				_ = conn.WriteMessage(serverws.TextMessage, []byte("observed"))
				if mode == "peer_close" {
					_ = conn.WriteControl(serverws.CloseMessage, serverws.FormatCloseMessage(serverws.CloseNormalClosure, ""), time.Now().Add(time.Second))
					return
				}
				if mode == "read_error" {
					return
				}
				if mode == "partial_message" {
					// A valid non-final frame followed by a lost TCP connection.
					_, _ = conn.UnderlyingConn().Write([]byte{0x01, 0x03, 'p', 'a', 'r'})
					return
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
			conn, response, err := lease.DialWebSocket(ctx, endpoint, fhttp.Header{}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if response.StatusCode != http.StatusSwitchingProtocols || budget.Remaining() != 0 || conn.Attempt().AccountID != 42 || conn.Attempt().Path.Status != attemptmeta.PathDirect {
				t.Fatalf("missing handshake identity: %+v remaining=%d", conn.Attempt(), budget.Remaining())
			}
			// Closing the synthetic handshake body cannot finalize an active socket.
			_ = response.Body.Close()
			if facts := PhysicalFacts(ctx); len(facts) != 0 {
				t.Fatalf("101 fabricated a completed exchange: %+v", facts)
			}
			_, payload, err := conn.ReadMessage()
			if err != nil || string(payload) != "observed" {
				t.Fatalf("payload=%q err=%v", payload, err)
			}
			wantOutcome, wantBytes := "closed", int64(8)
			switch mode {
			case "peer_close", "read_error", "partial_message":
				_, _, err = conn.ReadMessage()
				if err == nil {
					t.Fatal("expected close/read failure")
				}
				wantOutcome = "read_error"
				if mode == "peer_close" {
					wantOutcome = "peer_closed"
				}
				if mode == "partial_message" {
					wantBytes += 3
				}
			case "cancel":
				done := make(chan error, 1)
				go func() { _, _, err := conn.ReadMessage(); done <- err }()
				cancel()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("canceled read succeeded")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("cancel retained read")
				}
				wantOutcome = "canceled"
			}
			var closes sync.WaitGroup
			for i := 0; i < 8; i++ {
				closes.Add(1)
				go func() { defer closes.Done(); _ = conn.Close() }()
			}
			closes.Wait()
			facts := PhysicalFacts(ctx)
			if len(facts) != 1 || facts[0].Attempt.ID != conn.Attempt().ID || facts[0].Status != 101 || facts[0].HeaderOutcome != "upgraded" || facts[0].BodyOutcome != wantOutcome || facts[0].BodyBytes != wantBytes || facts[0].Usage.Found {
				t.Fatalf("physical outcome=%+v expected=%s/%d", facts, wantOutcome, wantBytes)
			}
			if mode != "cancel" {
				if _, _, err := lease.DialWebSocket(ctx, endpoint, nil, time.Second); !errors.Is(err, ErrPhysicalCallLimit) {
					t.Fatalf("second handshake bypassed budget: %v", err)
				}
				if handshakes.Load() != 1 {
					t.Fatal("budget checked after network submission")
				}
			}
			ConfirmPhysicalFacts(ctx, facts)
			if len(PhysicalFacts(ctx)) != 0 {
				t.Fatal("repeated physical acknowledgement")
			}
			if lease.clientHandle != nil {
				lease.clientHandle.mu.Lock()
				refs := lease.clientHandle.refs
				lease.clientHandle.mu.Unlock()
				if refs != 0 {
					t.Fatalf("retained %d client request references", refs)
				}
			}
		})
	}
}

func TestWebSocketRejectionBodyAndConnectionRetryConsumeBudget(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		ctx, _ := webSocketLedgerContext(t, 1)
		lease, _ := webSocketTestLease(t, ctx)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429); _, _ = io.WriteString(w, "limit") }))
		defer server.Close()
		_, response, err := lease.DialWebSocket(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil, time.Second)
		if !errors.Is(err, websocket.ErrBadHandshake) {
			t.Fatalf("handshake error=%v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		facts := PhysicalFacts(ctx)
		if len(facts) != 1 || facts[0].Status != 429 || facts[0].HeaderOutcome != "client_error" || facts[0].BodyBytes != 5 || facts[0].BodyOutcome != "eof" {
			t.Fatalf("lost rejection: %+v", facts)
		}
	})
	t.Run("connection_retry", func(t *testing.T) {
		ctx, budget := webSocketLedgerContext(t, 2)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		browser, err := newBrowserClient("socks5://"+address, "")
		if err != nil {
			t.Fatal(err)
		}
		defer browser.CloseIdleConnections()
		lease := &Lease{browser: browser, proxyPool: true, NodeID: 9}
		_, _, err = lease.DialWebSocket(ctx, "ws://example.invalid", nil, time.Second)
		facts := PhysicalFacts(ctx)
		if !errors.Is(err, ErrPhysicalCallLimit) || len(facts) != 2 || budget.Remaining() != 0 {
			t.Fatalf("internal retry bypassed total budget: err=%v facts=%+v remaining=%d", err, facts, budget.Remaining())
		}
		if facts[0].Attempt.ID == facts[1].Attempt.ID || facts[1].Stage != "connection_retry" || facts[1].Attempt.Path.NodeID != 9 {
			t.Fatalf("retry identity lost: %+v", facts)
		}
		if _, penalized := feedbackKind(domainegress.ScopeWeb, 0, err); penalized {
			t.Fatal("local budget rejection penalized egress")
		}
	})
}
