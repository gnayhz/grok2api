package selector

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAccountHealthConcurrentLeaseResults(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	create := func(t *testing.T) account.Credential {
		v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, SourceKey: t.Name(), Name: t.Name(), Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	get := func(t *testing.T, id uint64) account.Credential {
		v, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	t.Run("two failures from same selected snapshot accumulate", func(t *testing.T) {
		selected := create(t)
		selector.MarkFailure(ctx, selected, http.StatusTooManyRequests, 0)
		selector.MarkFailure(ctx, selected, http.StatusTooManyRequests, 0)
		if got := get(t, selected.ID).FailureCount; got != 2 {
			t.Fatalf("durable failures=%d, want 2", got)
		}
	})
	t.Run("late success preserves newer failure", func(t *testing.T) {
		initial := create(t)
		selector.MarkFailure(ctx, initial, http.StatusTooManyRequests, 0)
		selected := get(t, initial.ID)
		selector.MarkFailure(ctx, selected, http.StatusTooManyRequests, 0)
		selector.MarkSuccessWithRecovery(ctx, selected, nil)
		if got := get(t, selected.ID); got.FailureCount != 2 || got.CooldownUntil == nil {
			t.Fatalf("late success erased newer failure: failures=%d cooldown=%v", got.FailureCount, got.CooldownUntil)
		}
	})
	t.Run("quality idle projection preserves current failures", func(t *testing.T) {
		selected := create(t)
		selector.MarkFailure(ctx, selected, http.StatusTooManyRequests, 0)
		if err := selector.MarkQualityIdleFailure(ctx, selected, time.Minute); err != nil {
			t.Fatal(err)
		}
		persisted := get(t, selected.ID)
		projected := selector.applyRoutingHealth(persisted, time.Now())
		if persisted.FailureCount != 1 || projected.FailureCount != 1 {
			t.Fatalf("SQL failures=%d projection=%d, want both 1", persisted.FailureCount, projected.FailureCount)
		}
	})
	t.Run("late stream soft failure preserves newer long cooldown", func(t *testing.T) {
		selected := create(t)
		selector.MarkFailure(ctx, selected, http.StatusTooManyRequests, time.Hour)
		before := get(t, selected.ID)
		if err := selector.MarkFailureAfterSuccess(ctx, selected, 0, 0); err != nil {
			t.Fatal(err)
		}
		got := get(t, selected.ID)
		if got.FailureCount != 1 || got.CooldownUntil == nil || !got.CooldownUntil.Equal(*before.CooldownUntil) || got.LastError != before.LastError {
			t.Fatalf("late soft failure shortened newer restriction: failures=%d cooldown=%v marker=%s", got.FailureCount, got.CooldownUntil, got.LastError)
		}
	})
}

func TestAccountHealthProjectionUsesSQLRevision(t *testing.T) {
	s := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	until := now.Add(time.Hour)
	current := repository.InvalidationEvent{Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 1,
		HealthRevision: 8, Revision: 1, FailureCount: 3, CooldownUntil: &until, PublishedAt: now}
	s.ApplyInvalidation(current)
	old := current
	old.HealthRevision, old.Revision, old.FailureCount, old.CooldownUntil, old.PublishedAt = 7, 100, 0, nil, now.Add(time.Second)
	s.ApplyInvalidation(old)
	legacy := old
	legacy.HealthRevision = 0
	legacy.Revision = 101
	s.ApplyInvalidation(legacy)
	base := account.Credential{ID: 1, Provider: account.ProviderBuild, HealthRevision: 6}
	for _, got := range []account.Credential{s.applyRoutingHealth(base, now), applyHealthSnapshot(base, s.routingHealthSnapshot(account.ProviderBuild, now))} {
		if got.FailureCount != 3 || got.HealthRevision != 8 {
			t.Fatalf("late publication overwrote SQL state: revision=%d failures=%d", got.HealthRevision, got.FailureCount)
		}
	}
	base.HealthRevision = 9
	base.FailureCount = 4
	for _, got := range []account.Credential{s.applyRoutingHealth(base, now), applyHealthSnapshot(base, s.routingHealthSnapshot(account.ProviderBuild, now))} {
		if got.FailureCount != 4 || got.HealthRevision != 9 {
			t.Fatal("older overlay replaced newly loaded SQL snapshot")
		}
	}
}
