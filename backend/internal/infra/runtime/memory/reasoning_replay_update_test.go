package memory

import (
	"context"
	"testing"
	"time"
)

func TestReplayAtomicUpdateRespectsExpiryBudgetAndOwnership(t *testing.T) {
	ctx := context.Background()
	store := NewReasoningReplayStoreWithBudget(4, 800)
	now := time.Now()
	if err := store.Set(ctx, "m", "a", [][]byte{[]byte("expired")}, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	var retained [][]byte
	if err := store.Update(ctx, "m", "a", now.Add(time.Hour), func(old [][]byte) [][]byte {
		if len(old) != 0 {
			t.Error("expired history reappeared")
		}
		retained = [][]byte{make([]byte, 400)}
		return retained
	}); err != nil {
		t.Fatal(err)
	}
	retained[0][0] = 99
	got, _, _ := store.Get(ctx, "m", "a", now, time.Hour)
	if got[0][0] != 0 {
		t.Fatal("merge callback retained a mutable alias")
	}
	if err := store.Update(ctx, "m", "b", now.Add(time.Hour), func(old [][]byte) [][]byte { return [][]byte{make([]byte, 400)} }); err != nil {
		t.Fatal(err)
	}
	if store.totalBytes > store.maxBytes || len(store.values) != 1 {
		t.Fatal("atomic update bypassed byte eviction")
	}
	sum := int64(0)
	for _, e := range store.values {
		sum += reasoningReplayEntrySize(e)
	}
	if sum != store.totalBytes {
		t.Fatal("expired replacement leaked byte accounting")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Update(cancelled, "m", "b", now.Add(time.Hour), func(old [][]byte) [][]byte { t.Error("cancelled callback ran"); return nil }); err == nil {
		t.Fatal("cancelled update succeeded")
	}
}
