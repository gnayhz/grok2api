package egress

import (
	"context"
	"fmt"
	physical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// The same benchmark runs before/after physical WS observations. Measure an
// already-upgraded exchange; handshake, DPoP and SQL costs are excluded.
func BenchmarkWebSocketRoundTrip(b *testing.B) {
	for _, size := range []int{256, 64 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			upstream := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
				c, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				for {
					typ, data, err := c.ReadMessage()
					if err != nil {
						return
					}
					if err := c.WriteMessage(typ, data); err != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			client, err := newBrowserClientWithBudget("", "", nil)
			if err != nil {
				b.Fatal(err)
			}
			defer client.CloseIdleConnections()
			lease := &Lease{browser: client}
			ctx, _ := physical.WithTrace(context.Background())
			ctx = physical.WithPhysicalCallTrace(attemptmeta.WithRequest(ctx, "benchmark", 0, "", nil), testsupport.NewPhysicalJournalFactory().NewPhysicalJournal(), "console", "realtime")
			conn, _, err := lease.DialWebSocket(ctx, "ws"+strings.TrimPrefix(upstream.URL, "http"), nil, time.Second)
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close()
			payload := make([]byte, size)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
				if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
					b.Fatal(err)
				}
				if _, data, err := conn.ReadMessage(); err != nil || len(data) != size {
					b.Fatalf("bytes=%d err=%v", len(data), err)
				}
			}
			b.StopTimer()
		})
	}
}
