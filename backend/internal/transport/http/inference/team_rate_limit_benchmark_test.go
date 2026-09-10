package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func BenchmarkHTTPActiveTeamRateLimit(b *testing.B) {
	for _, stream := range []bool{false, true} {
		b.Run(fmt.Sprintf("stream=%t", stream), func(b *testing.B) {
			var calls atomic.Int32
			f := newResponseRetentionFixture(b, "sqlite", account.ProviderBuild, responseRetentionHooks{beforeResponse: func(w http.ResponseWriter, _ *http.Request) bool {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "600")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"code":"resource-exhausted","error":"Too many requests for team f1692451-874f-4765-ab9b-5285f6c6ff65 and model grok-4.5. Requests per Minute (actual/limit): 2/2."}`)
				return true
			}})
			body, err := json.Marshal(map[string]any{"model": f.model, "stream": stream, "store": false, "input": "hello"})
			if err != nil {
				b.Fatal(err)
			}
			send := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+f.secret)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				f.router.ServeHTTP(w, r)
				return w
			}
			if seed := send(); seed.Code != 429 || calls.Load() != 1 {
				b.Fatalf("initial throttle status=%d calls=%d", seed.Code, calls.Load())
			}
			b.ReportAllocs()
			b.ResetTimer()
			var last *httptest.ResponseRecorder
			for range b.N {
				last = send()
				if last.Code != 429 {
					b.Fatalf("active throttle status=%d", last.Code)
				}
			}
			b.StopTimer()
			if calls.Load() != 1 || f.generated.Load() != 0 || f.states.ownershipCalls.Load() != 0 || f.states.stateCalls.Load() != 0 {
				b.Fatal("active throttle generated or committed response state")
			}
			b.ReportMetric(0, "upstream-calls/op")
			b.ReportMetric(float64(last.Body.Len()), "wire-bytes/op")
		})
	}
}
