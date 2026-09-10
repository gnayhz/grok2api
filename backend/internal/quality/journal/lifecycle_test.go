package journal_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/journal"
)

func TestAdmittedCompletionSurvivesFullIncidentQueue(t *testing.T) {
	r, store := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	if err := r.DB().Create(&journal.CapacityRow{ID: 1, Limit: 1}).Error; err != nil {
		t.Fatal(err)
	}
	admission := event("admitted", now)
	admission.Outcome, admission.HoldUntil = "delivered", time.Time{}
	if err := store.Record(ctx, admission); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(ctx, event("incident", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckCapacity(ctx); !errors.Is(err, journal.ErrBacklogFull) {
		t.Fatalf("admission should be full: %v", err)
	}
	completion := admission
	completion.Stage, completion.Outcome = "completion", "completed"
	if err := store.Record(ctx, completion); err != nil {
		t.Fatalf("accepted request could not finish: %v", err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.InFlight != 0 || stats.Pending != 1 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if err := store.Record(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if claims, err := store.Claim(ctx, "worker", now, time.Minute, 10); err != nil || len(claims) != 1 || claims[0].Event.Attempt.ID != "incident" {
		t.Fatalf("archival facts required consumption: %+v %v", claims, err)
	}
}

func TestCompletionRecoveryDistinguishesLiveOwnerAndLateResult(t *testing.T) {
	r, live := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	admission := event("live", now)
	admission.Outcome, admission.HoldUntil = "delivered", time.Time{}
	if err := live.Record(ctx, admission); err != nil {
		t.Fatal(err)
	}
	other := journal.New(r.DB())
	later := now.Add(journal.CompletionLease + time.Second)
	if err := live.RenewCompletions(ctx, later); err != nil {
		t.Fatal(err)
	}
	if err := other.RecoverCompletions(ctx, later); err != nil {
		t.Fatal(err)
	}
	stats, err := other.Stats(ctx)
	if err != nil || stats.InFlight != 1 || stats.Unconfirmed != 0 {
		t.Fatalf("live request reclaimed: %+v %v", stats, err)
	}
	// No heartbeat from the original owner: another instance cannot renew it.
	later = later.Add(journal.CompletionLease + time.Second)
	if err := other.RenewCompletions(ctx, later); err != nil {
		t.Fatal(err)
	}
	if err := other.RecoverCompletions(ctx, later); err != nil {
		t.Fatal(err)
	}
	if err := other.RecoverCompletions(ctx, later); err != nil {
		t.Fatal(err)
	}
	stats, err = other.Stats(ctx)
	if err != nil || stats.InFlight != 0 || stats.Unconfirmed != 1 {
		t.Fatalf("lost completion not accounted: %+v %v", stats, err)
	}
	completion := admission
	completion.Stage, completion.Outcome = "completion", "completed"
	if err := live.Record(ctx, completion); err != nil {
		t.Fatalf("late real fact lost: %v", err)
	}
	var facts int64
	if err := r.DB().Model(&journal.EventRow{}).Count(&facts).Error; err != nil || facts != 3 {
		t.Fatalf("facts=%d err=%v", facts, err)
	}
}

func TestFailedCompletionStopsHeartbeatAndRemainsRecoverable(t *testing.T) {
	r, store := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	e := event("write-failed", now)
	e.Outcome, e.HoldUntil = "delivered", time.Time{}
	if err := store.Record(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := r.DB().Exec("CREATE TRIGGER fail_terminal BEFORE INSERT ON q_guard_event WHEN NEW.stage = 'completion' BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	e.Stage, e.Outcome = "completion", "completed"
	if err := store.Record(ctx, e); err == nil {
		t.Fatal("injected failure was ignored")
	}
	if err := r.DB().Exec("DROP TRIGGER fail_terminal").Error; err != nil {
		t.Fatal(err)
	}
	later := now.Add(journal.CompletionLease + time.Second)
	if err := store.RenewCompletions(ctx, later); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverCompletions(ctx, later); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.InFlight != 0 || stats.Unconfirmed != 1 {
		t.Fatalf("failed finalizer renewed forever: %+v %v", stats, err)
	}
}

func TestTransientCompletionFailureRetriesExactFact(t *testing.T) {
	r, store := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	e := event("retry-terminal", now)
	e.Outcome, e.HoldUntil = "delivered", time.Time{}
	if err := store.Record(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := r.DB().Exec("CREATE TRIGGER fail_terminal BEFORE INSERT ON q_guard_event WHEN NEW.stage = 'completion' BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	e.Stage, e.Outcome = "completion", "completed"
	if err := store.Record(ctx, e); err == nil {
		t.Fatal("failure ignored")
	}
	if err := r.DB().Exec("DROP TRIGGER fail_terminal").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.RetryCompletions(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// Replaying the actual fact checks that retries preserved its payload/time.
	if err := store.Record(ctx, e); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.InFlight != 0 || stats.Unconfirmed != 0 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestRetentionDrainsMultipleBatchesPreservingPendingAndActive(t *testing.T) {
	r, store := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	for start := 0; start < 2005; start += 100 {
		var batch []journal.Event
		for n := start; n < min(start+100, 2005); n++ {
			e := event(fmt.Sprintf("archive-%04d", n), old)
			e.Stage, e.Outcome, e.HoldUntil = "completion", "completed", time.Time{}
			batch = append(batch, e)
		}
		if err := store.RecordMany(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Record(ctx, event("pending", old)); err != nil {
		t.Fatal(err)
	}
	active := event("active", old)
	active.Outcome, active.HoldUntil = "delivered", time.Time{}
	if err := store.Record(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := store.Sweep(ctx, now); err != nil {
		t.Fatal(err)
	}
	var rows []journal.EventRow
	if err := r.DB().Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].AttemptID != "active" || rows[1].AttemptID != "pending" {
		t.Fatalf("retention lost live work or failed to drain: %+v", rows)
	}
}
