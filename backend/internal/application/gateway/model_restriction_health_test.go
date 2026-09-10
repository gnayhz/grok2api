package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestModelRestrictionInvalidationPreservesLatestHealth(t *testing.T) {
	for _, invalidation := range []string{"quota", "denial", "capability", "route"} {
		for _, cleared := range []bool{false, true} {
			t.Run(invalidation+"/"+map[bool]string{false: "new_cooldown", true: "cleared_cooldown"}[cleared], func(t *testing.T) {
				ctx := context.Background()
				db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-health.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if err := db.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
				repo := relational.NewAccountRepository(db)
				v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "health", SourceKey: "health", EncryptedAccessToken: "token", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
				if err != nil {
					t.Fatal(err)
				}
				selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Hour, time.Hour)
				if cleared {
					if _, err := repo.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthFailure, Status: 503, CooldownBase: time.Hour, CooldownMax: time.Hour}); err != nil {
						t.Fatal(err)
					}
				}
				repo.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { selector.ApplyInvalidation(e) })
				acquire := func(want bool) {
					t.Helper()
					lease, err := selector.Acquire(ctx, v.Provider, 0, "other-model", "", "", nil, false)
					if err == nil {
						lease.Release()
					}
					if (err == nil) != want {
						t.Fatalf("health eligibility=%v want=%v err=%v", err == nil, want, err)
					}
				}
				acquire(!cleared) // Warm the base snapshot before the health transition.
				if cleared {
					if _, err := repo.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthClearCooldown}); err != nil {
						t.Fatal(err)
					}
				} else {
					selector.MarkFailure(ctx, v, 503, time.Hour)
				}
				acquire(cleared)
				switch invalidation {
				case "quota":
					selector.MarkModelQuotaExhausted(ctx, v, nil, "limited-model", time.Hour)
				case "denial":
					if err := selector.MarkModelAccessDenied(ctx, v, "limited-model", time.Hour); err != nil {
						t.Fatal(err)
					}
				case "capability":
					selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, Provider: v.Provider, AccountID: v.ID})
				case "route":
					selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: v.Provider})
				}
				acquire(cleared)
			})
		}
	}

}
