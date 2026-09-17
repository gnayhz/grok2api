package gateway

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
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
				sel := selector.NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Hour, time.Hour)
				if cleared {
					if _, err := repo.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthFailure, Status: 503, CooldownBase: time.Hour, CooldownMax: time.Hour}); err != nil {
						t.Fatal(err)
					}
				}
				repo.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { sel.ApplyInvalidation(e) })
				acquire := func(want bool) {
					t.Helper()
					acquireErr := func() error {
						session, sessionErr := sel.BeginSelectionSessionForKey(ctx, v.Provider, 0, "other-model", "", "", nil, false, clientkeydomain.AccountScope{})
						if sessionErr != nil {
							return sessionErr
						}
						lease, leaseErr := session.Acquire(ctx, nil, false)
						if leaseErr != nil {
							return leaseErr
						}
						lease.Release()
						return nil
					}()
					if (acquireErr == nil) != want {
						t.Fatalf("health eligibility=%v want=%v err=%v", acquireErr == nil, want, acquireErr)
					}
				}
				acquire(!cleared) // Warm the base snapshot before the health transition.
				if cleared {
					if _, err := repo.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthClearCooldown}); err != nil {
						t.Fatal(err)
					}
				} else {
					sel.MarkFailure(ctx, v, 503, time.Hour)
				}
				acquire(cleared)
				switch invalidation {
				case "quota":
					sel.MarkModelQuotaExhausted(ctx, v, nil, "limited-model", time.Hour)
				case "denial":
					if err := sel.MarkModelAccessDenied(ctx, v, "limited-model", time.Hour); err != nil {
						t.Fatal(err)
					}
				case "capability":
					sel.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, Provider: v.Provider, AccountID: v.ID})
				case "route":
					sel.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: v.Provider})
				}
				acquire(cleared)
			})
		}
	}

}
