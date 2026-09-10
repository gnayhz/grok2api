package evidence

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func BenchmarkRecordAndSnapshot(b *testing.B) {
	store := openTestStoreB(b)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Minute)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		obs := obsAt(base.Add(time.Duration(i)*time.Microsecond), uint64(i%50), uint64(i%20), 0, model.OutcomeDegraded)
		if err := store.Record(ctx, obs); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	snapshot := store.SnapshotWindow(time.Now().UTC())
	estimate := store.CrossValidate(snapshot)
	if len(estimate.Exits) == 0 {
		b.Fatal("估计为空")
	}
}

func BenchmarkSnapshotEstimate(b *testing.B) {
	store := openTestStoreB(b)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 2000; i++ {
		obs := obsAt(base.Add(time.Duration(i)*time.Millisecond), uint64(i%50), uint64(i%20), 0, model.OutcomeDegraded)
		if err := store.Record(ctx, obs); err != nil {
			b.Fatal(err)
		}
	}
	now := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		estimate := store.CrossValidate(store.SnapshotWindow(now))
		_ = estimate
	}
}
