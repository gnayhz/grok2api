package accountsync_test

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkInitialSyncPipeline(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, warm := range []bool{false, true} {
			name := "initial"
			if warm {
				name = "existing"
			}
			b.Run(dialect+"/"+name, func(b *testing.B) {
				f := newInitialFixture(b, dialect, nil)
				id := f.account(b, "existing")
				ctx := context.Background()
				if warm {
					assertInitial(b, f.service.Sync(ctx, id), 1, 0)
				}
				f.billingCalls.Store(0)
				f.modelCalls.Store(0)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if !warm {
						id = f.account(b, fmt.Sprintf("new-%d", i))
					}
					result := f.service.Sync(ctx, id)
					if result.Succeeded != 1 || result.Failed != 0 {
						b.Fatalf("result=%+v", result)
					}
				}
				b.StopTimer()
				expected := int64(b.N)
				if warm {
					expected = 0
				}
				if f.billingCalls.Load() != expected || f.modelCalls.Load() != expected {
					b.Fatalf("requests billing=%d models=%d want=%d", f.billingCalls.Load(), f.modelCalls.Load(), expected)
				}
			})
		}
	}
}
