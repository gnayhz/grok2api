package journal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/journal"
)

func TestBacklogCapacityIsAtomicIdempotentAndResumesAfterAck(t *testing.T) {
	r, store := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	if err := r.DB().Create(&journal.CapacityRow{ID: 1, Limit: 2}).Error; err != nil {
		t.Fatal(err)
	}
	first := event("capacity-first", now)
	if err := store.Record(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := journal.New(r.DB()).Record(ctx, event("capacity-second", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(ctx, first); err != nil {
		t.Fatalf("idempotent receipt consumed capacity: %v", err)
	}
	third := event("capacity-third", now)
	if err := store.Record(ctx, third); !errors.Is(err, journal.ErrBacklogFull) {
		t.Fatalf("full backlog err=%v", err)
	}
	for _, table := range []string{"q_guard_event", "q_guard_outbox", "q_guard_restriction"} {
		var count int64
		if err := r.DB().Table(table).Count(&count).Error; err != nil || count != 2 {
			t.Fatalf("rejected event partially persisted %s: %d %v", table, count, err)
		}
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Pending != 2 || stats.Limit != 2 || stats.OldestAt == nil {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	claims, err := store.Claim(ctx, "capacity-worker", now, time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, claims[0], now); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, claims[0], now); err == nil {
		t.Fatal("duplicate ack succeeded")
	}
	if err := store.Record(ctx, third); err != nil {
		t.Fatalf("ack did not release capacity: %v", err)
	}
	var row journal.CapacityRow
	if err := r.DB().First(&row, "id = 1").Error; err != nil || row.Pending != 2 {
		t.Fatalf("capacity drift %+v %v", row, err)
	}
}

func TestCapacityUpgradeCountsExistingPendingWork(t *testing.T) {
	r, store := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	if err := store.Record(ctx, event("existing", now)); err != nil {
		t.Fatal(err)
	}
	if err := r.DB().Where("id = 1").Delete(&journal.CapacityRow{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := journal.New(r.DB()).Record(ctx, event("after-upgrade", now)); err != nil {
		t.Fatal(err)
	}
	var row journal.CapacityRow
	if err := r.DB().First(&row, "id = 1").Error; err != nil || row.Pending != 2 {
		t.Fatalf("upgrade undercounted %+v %v", row, err)
	}
}
