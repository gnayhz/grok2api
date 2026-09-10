package inference

import (
	"fmt"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func BenchmarkHTTPWebResponseStorage(b *testing.B) {
	for _, streaming := range []bool{false, true} {
		for _, preference := range []string{"default", "false"} {
			b.Run(fmt.Sprintf("stream=%t/%s", streaming, preference), func(b *testing.B) {
				f := newResponseRetentionFixture(b, "sqlite", account.ProviderWeb)
				var fields map[string]any
				if preference == "false" {
					fields = map[string]any{"store": false}
				}
				// The before revision incorrectly stores explicit false. Probe and report
				// that real behavior, then require a stable number of commits per operation
				// in either revision; never benchmark a skipped generation or audit write.
				retentionResponse(b, f.create(b, streaming, fields, "cost"), streaming)
				ownerPerRequest, statePerRequest := f.states.ownershipCalls.Load(), f.states.stateCalls.Load()
				if f.generated.Load() != 1 || ownerPerRequest != statePerRequest || ownerPerRequest < 0 || ownerPerRequest > 1 {
					b.Fatal("invalid seed request")
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					output := f.create(b, streaming, fields, "cost")
					if output.Code != 200 {
						b.Fatalf("generation=%d: %s", output.Code, output.Body.String())
					}
				}
				b.StopTimer()
				total := int32(b.N + 1)
				if f.generated.Load() != total || f.states.ownershipCalls.Load() != total*ownerPerRequest || f.states.stateCalls.Load() != total*statePerRequest {
					b.Fatalf("actual operations generated=%d native=%d owner=%d expected=%d", f.generated.Load(), f.states.stateCalls.Load(), f.states.ownershipCalls.Load(), total)
				}
				record := f.lastAudit(b)
				if record.LedgerOutcome != "committed" || record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.ErrorCode != "" {
					b.Fatalf("incomplete request: %+v", record)
				}
				b.ReportMetric(1, "generations/op")
				b.ReportMetric(float64(ownerPerRequest), "owners/op")
				b.ReportMetric(float64(statePerRequest), "states/op")
			})
		}
	}
}
