package inference

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func BenchmarkHTTPConsoleQuotaRefresh(b *testing.B) {
	var calls, mints atomic.Int32
	f := newResponseRetentionFixture(b, "sqlite", account.ProviderConsole, responseRetentionHooks{
		beforeToken: func(*http.Request) { mints.Add(1) },
		beforeUsage: func(w http.ResponseWriter, r *http.Request) bool {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"quotas":[{"kind":"chat","limit":10,"used":1,"remaining":9},{"kind":"image","limit":5,"used":0,"remaining":5},{"kind":"video","limit":2,"used":0,"remaining":2}]}`)
			return true
		},
	})
	run := func() {
		windows, err := f.accountService.RefreshQuota(context.Background(), f.accountID)
		if err != nil || len(windows) != 3 {
			b.Fatalf("windows=%d err=%v", len(windows), err)
		}
	}
	run()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		run()
	}
	b.StopTimer()
	if calls.Load() != int32(b.N+1) || mints.Load() != 1 || f.generated.Load() != 0 {
		b.Fatalf("quota=%d mint=%d generated=%d", calls.Load(), mints.Load(), f.generated.Load())
	}
	b.ReportMetric(1, "quota_requests/op")
	b.ReportMetric(0, "mints/op")
	b.ReportMetric(0, "generations/op")
}
