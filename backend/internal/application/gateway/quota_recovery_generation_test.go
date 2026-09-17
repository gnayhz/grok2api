package gateway

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestQuotaRecoveryLateRequestDoesNotChangeNewState(t *testing.T) {
	for _, scenario := range []string{"probe_success_after_new_exhaustion", "exhaustion_after_reset"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "recovery.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			sel := selector.NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
			v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "recovery", SourceKey: "recovery", EncryptedAccessToken: "token", Enabled: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "exhaustion_after_reset" {
				if err := repo.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
					t.Fatal(err)
				}
				sel.MarkFreeQuotaExhausted(ctx, v, 100, 100)
				if _, err := repo.GetQuotaRecovery(ctx, v.ID); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("late pre-reset exhaustion recreated old quota state: %v", err)
				}
				return
			}
			now := time.Now().UTC()
			due := now.Add(-time.Minute)
			if err := testsupport.Recovery(ctx, repo, account.QuotaRecovery{AccountID: v.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted, NextProbeAt: &due, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			session, sessionErr := sel.BeginSelectionSessionForKey(ctx, v.Provider, 0, "model", "", "", map[uint64]bool{}, true, clientkeydomain.AccountScope{})
			if sessionErr != nil {
				t.Fatal(sessionErr)
			}
			lease, err := session.Acquire(ctx, map[uint64]bool{}, true)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if !lease.QuotaProbe {
				t.Fatal("did not claim probe")
			}
			current, err := repo.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			sel.MarkFreeQuotaExhausted(ctx, current, 200, 200)
			sel.MarkSuccessWithRecovery(ctx, lease.Credential, lease.QuotaRecoveryRef)
			got, err := repo.GetQuotaRecovery(ctx, v.ID)
			if err != nil || got.ConfirmedUsed != 200 {
				t.Fatalf("late probe success erased newer exhaustion: used=%d err=%v", got.ConfirmedUsed, err)
			}
		})
	}
}

func TestQuotaProbeSelectionPathsCarryClaimAndFinishRevision(t *testing.T) {
	for _, path := range []string{"ordinary", "session", "pinned"} {
		for _, outcome := range []string{"stale_success", "success_then_failure", "unrelated_success"} {
			t.Run(path+"/"+outcome, func(t *testing.T) {
				ctx := context.Background()
				db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "probe-path.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if err := db.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
				repo := relational.NewAccountRepository(db)
				limiter := memory.NewConcurrencyLimiter()
				sel := selector.NewSelector(repo, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
				v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "path", SourceKey: "path", EncryptedAccessToken: "token", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
				if err != nil {
					t.Fatal(err)
				}
				seed, err := repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: time.Now().Add(-25 * time.Hour), Used: 100, Limit: 100})
				if err != nil || !seed.Applied {
					t.Fatal(err)
				}
				var lease *selector.Lease
				switch path {
				case "ordinary":
					var session *selector.SelectionSession
					session, err = sel.BeginSelectionSessionForKey(ctx, v.Provider, 0, "model", "", "", map[uint64]bool{}, true, clientkeydomain.AccountScope{})
					if err == nil {
						lease, err = session.Acquire(ctx, map[uint64]bool{}, true)
					}
				case "pinned":
					lease, err = sel.AcquirePinnedForKey(ctx, v.Provider, v.ID, 0, "model", "", true, clientkeydomain.AccountScope{})
				case "session":
					var session *selector.SelectionSession
					session, err = sel.BeginSelectionSessionForKey(ctx, v.Provider, 0, "model", "", "", map[uint64]bool{}, true, clientkeydomain.AccountScope{})
					if err == nil {
						lease, err = session.Acquire(ctx, map[uint64]bool{}, true)
					}
				}
				if err != nil || lease == nil {
					t.Fatalf("acquire: %v", err)
				}
				defer lease.Release()
				if lease.QuotaRecoveryRef == nil || lease.QuotaRecoveryRef.Revision != seed.Ref.Revision+1 || lease.Credential.QuotaRecoveryRevision != lease.QuotaRecoveryRef.Revision {
					t.Fatal("selection detached claim reference")
				}
				switch outcome {
				case "stale_success":
					sel.MarkFreeQuotaExhausted(ctx, lease.Credential, 200, 200)
					// An unrelated credential reload can return the new revision. The explicit
					// claim still prevents this old request from adopting it for completion.
					current, err := repo.Get(ctx, v.ID)
					if err != nil {
						t.Fatal(err)
					}
					sel.MarkSuccessWithRecovery(ctx, current, lease.QuotaRecoveryRef)
				case "success_then_failure":
					completed := sel.MarkSuccessWithRecovery(ctx, lease.Credential, lease.QuotaRecoveryRef)
					if completed.QuotaRecoveryRevision != lease.QuotaRecoveryRef.Revision+1 {
						t.Fatal("successful completion lost own next revision")
					}
					sel.MarkFreeQuotaExhausted(ctx, completed, 200, 200)
				case "unrelated_success":
					sel.MarkSuccess(ctx, lease.Credential)
				}
				recovery, err := repo.GetQuotaRecovery(ctx, v.ID)
				if err != nil {
					t.Fatal(err)
				}
				if outcome == "unrelated_success" {
					if recovery.Status != account.QuotaRecoveryStatusProbing {
						t.Fatal("unrelated success changed probe")
					}
				} else if recovery.ConfirmedUsed != 200 {
					t.Fatal("completion erased real exhaustion")
				}
				lease.Release()
				current, err := limiter.Current(ctx, repository.AccountConcurrencyKey(v.ID))
				if err != nil || current != 0 {
					t.Fatalf("leaked capacity=%d err=%v", current, err)
				}
			})
		}
	}
}
