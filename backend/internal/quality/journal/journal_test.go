package journal_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func setup(t *testing.T) (*registry.Registry, *journal.Store) {
	t.Helper()
	r, err := registry.Open(context.Background(), registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, journal.New(r.DB())
}
func event(id string, now time.Time) model.Event {
	return model.Event{Attempt: attemptmeta.Identity{ID: id, RequestID: "request", AccountID: 7, Provider: "grok_build", Revision: 4,
		Path: attemptmeta.Path{NodeID: 9, Epoch: 3, Status: attemptmeta.PathRegistered}}, Stage: "admission", Outcome: "degraded", At: now, HoldUntil: now.Add(time.Minute)}
}
func allowed(t *testing.T, s *journal.Store, want bool, now time.Time) {
	t.Helper()
	got, err := s.AccountAllowed(context.Background(), 7, now)
	if err != nil || got != want {
		t.Fatalf("allowed=%v want=%v err=%v", got, want, err)
	}
}

func TestEventRestrictionAndOutboxAreAtomic(t *testing.T) {
	r, s := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	if err := r.DB().Exec("CREATE TRIGGER fail_guard_outbox BEFORE INSERT ON q_guard_outbox BEGIN SELECT RAISE(ABORT, 'injected outbox failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, event("attempt-1", now)); err == nil {
		t.Fatal("injected transaction failure returned receipt")
	}
	for _, table := range []string{"q_guard_event", "q_guard_restriction", "q_guard_outbox"} {
		var count int64
		if err := r.DB().Table(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("partial commit in %s: count=%d err=%v", table, count, err)
		}
	}
	allowed(t, s, true, now)
	if err := r.DB().Exec("DROP TRIGGER fail_guard_outbox").Error; err != nil {
		t.Fatal(err)
	}
	e := event("attempt-1", now)
	if err := s.Record(ctx, e); err != nil {
		t.Fatal(err)
	}
	allowed(t, s, false, now)
	if err := s.Record(ctx, e); err != nil {
		t.Fatal(err)
	}
	e.HoldUntil = e.HoldUntil.Add(time.Minute)
	if err := s.Record(ctx, e); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("replay mutated expiry: %v", err)
	}
	allowed(t, s, true, now.Add(time.Minute))
}

func TestRestrictionsHaveIndependentOwnersAndCrossReplicaAuthority(t *testing.T) {
	r, first := setup(t)
	second := journal.New(r.DB())
	ctx, now := context.Background(), time.Now().UTC()
	for _, id := range []string{"a", "b"} {
		if err := first.Record(ctx, event(id, now)); err != nil {
			t.Fatal(err)
		}
	}
	allowed(t, second, false, now)
	if err := first.Release(ctx, "a/admission", now); err != nil {
		t.Fatal(err)
	}
	allowed(t, second, false, now)
	if err := first.Release(ctx, "b/admission", now); err != nil {
		t.Fatal(err)
	}
	allowed(t, second, true, now)
	if err := r.TransitionAccount(ctx, model.AccountTransitionRequest{AccountID: 7, To: model.AccountRemanded, CaseID: 42}); err != nil {
		t.Fatal(err)
	}
	allowed(t, second, false, now)
	if err := first.Release(ctx, "a/admission", now); err != nil {
		t.Fatal(err)
	}
	allowed(t, second, false, now.Add(time.Hour))
}
