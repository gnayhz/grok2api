package inference

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func BenchmarkHTTPConsoleDPoPSession(b *testing.B) {
	for _, mode := range []string{"cached", "refresh401"} {
		b.Run(mode, func(b *testing.B) {
			var mintCalls, responseCalls atomic.Int32
			var rejectOnce atomic.Bool
			f := newResponseRetentionFixture(b, "sqlite", account.ProviderConsole, responseRetentionHooks{
				beforeToken: func(*http.Request) { mintCalls.Add(1) },
				beforeResponse: func(w http.ResponseWriter, r *http.Request) bool {
					responseCalls.Add(1)
					if rejectOnce.Swap(false) {
						w.WriteHeader(http.StatusUnauthorized)
						return true
					}
					return false
				},
			})
			fields := map[string]any{"store": false}
			retentionResponse(b, f.create(b, false, fields, "cost"), false)
			if mintCalls.Load() != 1 || responseCalls.Load() != 1 || f.generated.Load() != 1 {
				b.Fatal("incomplete seed")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if mode == "refresh401" {
					rejectOnce.Store(true)
				}
				output := f.create(b, false, fields, "cost")
				if output.Code != 200 {
					b.Fatalf("status=%d: %s", output.Code, output.Body.String())
				}
			}
			b.StopTimer()
			mintPerRequest, responsePerRequest := int32(0), int32(1)
			if mode == "refresh401" {
				mintPerRequest, responsePerRequest = 1, 2
			}
			if mintCalls.Load() != 1+int32(b.N)*mintPerRequest || responseCalls.Load() != 1+int32(b.N)*responsePerRequest || f.generated.Load() != 1+int32(b.N) {
				b.Fatalf("mint=%d responses=%d generation=%d", mintCalls.Load(), responseCalls.Load(), f.generated.Load())
			}
			record := f.lastAudit(b)
			if record.ErrorCode != "" || record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" || record.InputTokens != 20 || record.OutputTokens != 5 {
				b.Fatalf("completion lost: %+v", record)
			}
			b.ReportMetric(float64(mintPerRequest), "mints/op")
			b.ReportMetric(float64(responsePerRequest), "responses/op")
			b.ReportMetric(1, "generations/op")
		})
	}
}
