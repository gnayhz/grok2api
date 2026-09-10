package browsertransport

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
)

func TestTLSHandshakeTraceIncludesHTTPAndWebSocket(t *testing.T) {
	for _, ws := range []bool{false, true} {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
		server.EnableHTTP2 = true
		server.StartTLS()
		tr := trustedTransport(t, server, Config{})
		var starts, ends atomic.Int32
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			TLSHandshakeStart: func() { starts.Add(1) },
			TLSHandshakeDone: func(state tls.ConnectionState, err error) {
				ends.Add(1)
				if err != nil || !state.HandshakeComplete || len(state.PeerCertificates) == 0 {
					t.Errorf("trace missing handshake result: complete=%v error=%v", state.HandshakeComplete, err)
				}
				want := "h2"
				if ws {
					want = "http/1.1"
				}
				if state.NegotiatedProtocol != want && !(ws && state.NegotiatedProtocol == "") {
					t.Errorf("ALPN=%s want=%s", state.NegotiatedProtocol, want)
				}
			},
		})
		if ws {
			conn, err := tr.DialWebSocketTLS(ctx, "tcp", strings.TrimPrefix(server.URL, "https://"))
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
		} else {
			for range 2 {
				req, _ := fhttp.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
				res, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
			}
		}
		tr.CloseIdleConnections()
		server.Close()
		if starts.Load() != 1 || ends.Load() != 1 {
			t.Fatalf("ws=%v starts=%d ends=%d", ws, starts.Load(), ends.Load())
		}
	}
}
