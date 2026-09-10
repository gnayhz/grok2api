package inference

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func BenchmarkHTTPResponseEncoding(b *testing.B) {
	for _, stream := range []bool{false, true} {
		for _, compressed := range []bool{false, true} {
			b.Run(fmt.Sprintf("stream=%t/gzip=%t", stream, compressed), func(b *testing.B) {
				f := newResponseRetentionFixture(b, "sqlite", account.ProviderWeb, responseRetentionHooks{middleware: []gin.HandlerFunc{middleware.Gzip()}})
				body, _ := json.Marshal(map[string]any{"model": f.model, "stream": stream, "input": "hello"})
				send := func() *httptest.ResponseRecorder {
					r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
					r.Header.Set("Authorization", "Bearer "+f.secret)
					r.Header.Set("Content-Type", "application/json")
					if compressed {
						r.Header.Set("Accept-Encoding", "gzip")
					}
					w := httptest.NewRecorder()
					f.router.ServeHTTP(w, r)
					return w
				}
				seed := send()
				if compressed && !stream {
					zr, err := gzip.NewReader(bytes.NewReader(seed.Body.Bytes()))
					if err != nil {
						b.Fatal(err)
					}
					decoded, err := io.ReadAll(zr)
					_ = zr.Close()
					if err != nil || !json.Valid(decoded) {
						b.Fatal("invalid compressed seed response")
					}
				}
				if seed.Code != 200 || f.generated.Load() != 1 || f.states.ownershipCalls.Load() != 1 || f.states.stateCalls.Load() != 1 {
					b.Fatal("invalid seed generation/storage")
				}
				b.ReportAllocs()
				b.ResetTimer()
				var last *httptest.ResponseRecorder
				for range b.N {
					last = send()
					if last.Code != 200 {
						b.Fatalf("generation status=%d", last.Code)
					}
				}
				b.StopTimer()
				total := int32(b.N + 1)
				if f.generated.Load() != total || f.states.ownershipCalls.Load() != total || f.states.stateCalls.Load() != total {
					b.Fatal("generation/storage operations changed")
				}
				record := f.lastAudit(b)
				if record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" || record.ErrorCode != "" {
					b.Fatal("incomplete generation/delivery/ledger")
				}
				// The old revision undercounts gzip bytes. Report the behavior rather
				// than rejecting the baseline whose cost is being measured.
				b.ReportMetric(float64(record.DeliveredBytes), "recorded-bytes/op")
				b.ReportMetric(float64(last.Body.Len()), "wire-bytes/op")
				b.ReportMetric(1, "generations/op")
				b.ReportMetric(1, "owners/op")
				b.ReportMetric(1, "states/op")
			})
		}
	}
}
