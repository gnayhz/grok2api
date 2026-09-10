package cli

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func BenchmarkHTTPBuildControlDocument(b *testing.B) {
	for _, operation := range []string{"models", "billing", "refresh"} {
		b.Run(operation, func(b *testing.B) {
			f := newControlDocumentFixture(b, "sqlite", operation)
			f.phase.Store(2)
			path := fmt.Sprintf("/accounts/%d/refresh-token", f.accountID)
			callsPerOp := int64(1)
			if operation == "billing" {
				path = fmt.Sprintf("/accounts/%d/refresh-billing", f.accountID)
				callsPerOp = 2
			}
			run := func() {
				if operation == "models" {
					if _, err := f.catalog.SyncAccount(context.Background(), f.accountID); err != nil {
						b.Fatal(err)
					}
				} else if output := f.request(http.MethodPost, path); output.Code != http.StatusOK {
					b.Fatalf("control status=%d", output.Code)
				}
			}
			run()
			beforeCalls := f.calls.Load()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run()
			}
			b.StopTimer()
			if f.calls.Load()-beforeCalls != int64(b.N)*callsPerOp || f.generated.Load() != 0 {
				b.Fatal("control call count changed or inference was generated")
			}
			b.ReportMetric(float64(callsPerOp), "upstream_calls/op")
			b.ReportMetric(0, "generations/op")
		})
	}
}
