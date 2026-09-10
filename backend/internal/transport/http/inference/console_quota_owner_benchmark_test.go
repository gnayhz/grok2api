package inference

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// Exercise both account entry points through the real Console protocol and SQL.
// Queue the normal recovery event as well as persisting the returned snapshot.
func BenchmarkHTTPConsoleQuotaTiming(b *testing.B) {
	for _, entry := range []string{"full", "mode"} {
		for _, exhausted := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/exhausted=%t", entry, exhausted), func(b *testing.B) {
				var calls, mints atomic.Int64
				f := newResponseRetentionFixture(b, "sqlite", account.ProviderConsole, responseRetentionHooks{
					beforeToken: func(*http.Request) { mints.Add(1) },
					beforeUsage: func(w http.ResponseWriter, _ *http.Request) bool {
						calls.Add(1)
						remaining := 9
						if exhausted {
							remaining = 0
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprintf(w, `{"quotas":[{"kind":"chat","limit":10,"remaining":%d},{"kind":"image","limit":5,"remaining":5},{"kind":"video","limit":2,"remaining":2}]}`, remaining)
						return true
					},
				})
				f.accountService.SetQuotaRecoveryQueue(memory.NewQuotaRecoveryQueue())
				run := func() {
					if entry == "mode" {
						window, err := f.accountService.RefreshQuotaMode(context.Background(), f.accountID, "console")
						if err != nil || window.Mode != "console" {
							b.Fatalf("window=%+v err=%v", window, err)
						}
					} else {
						windows, err := f.accountService.RefreshQuota(context.Background(), f.accountID)
						if err != nil || len(windows) != 3 {
							b.Fatalf("windows=%d err=%v", len(windows), err)
						}
					}
				}
				run()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					run()
				}
				b.StopTimer()
				if calls.Load() != int64(b.N+1) || mints.Load() != 1 || f.generated.Load() != 0 {
					b.Fatalf("quota=%d mint=%d generated=%d", calls.Load(), mints.Load(), f.generated.Load())
				}
				b.ReportMetric(1, "quota_requests/op")
				b.ReportMetric(0, "mints/op")
				b.ReportMetric(0, "generations/op")
			})
		}
	}
}
