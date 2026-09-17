package accountsync

import (
	"context"
	"testing"
)

type accountProgressSynchronizerStub struct{}

func (*accountProgressSynchronizerStub) Sync(context.Context, ...uint64) Result { panic("unused") }
func (s *accountProgressSynchronizerStub) SyncStream(ctx context.Context, ids <-chan uint64) Result {
	return s.SyncStreamObserved(ctx, ids, func(int, int) {})
}
func (*accountProgressSynchronizerStub) SyncStreamObserved(_ context.Context, ids <-chan uint64, progress func(int, int)) Result {
	n := 0
	for range ids {
		n++
	}
	for i := 1; i <= n; i++ {
		progress(i, i)
	}
	return Result{Succeeded: n}
}
func TestAccountSyncPipelineUsesFinalQueuedTotal(t *testing.T) {
	syncer := &accountProgressSynchronizerStub{}
	progress := make([][2]int, 0, 5)
	pipeline := startSyncPipeline(context.Background(), syncer, func(completed, total int) {
		progress = append(progress, [2]int{completed, total})
	})

	for _, accountID := range []uint64{11, 12, 13} {
		if err := pipeline.Observe(accountID); err != nil {
			t.Fatal(err)
		}
	}
	result := pipeline.Finish(false)

	if result.Succeeded != 3 {
		t.Fatalf("result = %#v", result)
	}
	if len(progress) == 0 || progress[len(progress)-1] != [2]int{3, 3} {
		t.Fatalf("progress = %#v", progress)
	}
	for _, value := range progress {
		if value[1] != 3 {
			t.Fatalf("progress contains changing total: %#v", progress)
		}
	}
}
