package relational

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func newRecoveryAccount(t *testing.T, r *AccountRepository, key string) account.Credential {
	t.Helper()
	v, _, err := r.UpsertByIdentity(context.Background(), account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, SourceKey: key, Name: key, EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", Enabled: true, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestQuotaRecoveryGenerationsAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v := newRecoveryAccount(t, ra, "generations")
			now := time.Now().UTC().Truncate(time.Microsecond)
			apply := func(r *AccountRepository, ref account.QuotaRecoveryRef, event account.RecoveryEvent, want bool) account.RecoveryResult {
				t.Helper()
				result, err := r.ApplyQuotaRecovery(ctx, ref, event)
				if err != nil || result.Applied != want {
					t.Fatalf("%s applied=%v want=%v err=%v", event.Kind, result.Applied, want, err)
				}
				return result
			}
			exhausted := account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: now.Add(-25 * time.Hour), Used: 100, Limit: 100}
			first := apply(ra, v.QuotaRecoveryRef(), exhausted, true)
			second := apply(rb, v.QuotaRecoveryRef(), exhausted, true)
			if second.Ref.Revision != first.Ref.Revision+1 {
				t.Fatal("concurrent negatives did not each advance version")
			}
			claimA := apply(ra, second.Ref, account.RecoveryEvent{Kind: account.RecoveryProbeClaimed, OccurredAt: now}, true)
			apply(rb, second.Ref, account.RecoveryEvent{Kind: account.RecoveryProbeClaimed, OccurredAt: now}, false)
			apply(rb, claimA.Ref, account.RecoveryEvent{Kind: account.RecoveryProbeClaimed, OccurredAt: now.Add(time.Minute)}, false)
			claimB := apply(rb, claimA.Ref, account.RecoveryEvent{Kind: account.RecoveryProbeClaimed, OccurredAt: now.Add(account.QuotaProbeLease)}, true)
			apply(ra, claimA.Ref, account.RecoveryEvent{Kind: account.RecoveryFreeProbeSucceeded}, false)
			clear := apply(rb, claimB.Ref, account.RecoveryEvent{Kind: account.RecoveryFreeProbeSucceeded}, true)
			if clear.Recovery != nil || clear.ResetRevision != clear.Ref.Revision {
				t.Fatal("successful probe lost persistent clear barrier")
			}
			apply(ra, claimA.Ref, exhausted, false)
			next := apply(ra, clear.Ref, exhausted, true)
			if err := rb.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
				t.Fatal(err)
			}
			apply(ra, next.Ref, exhausted, false)
			current, err := rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			absentRef := current.QuotaRecoveryRef()
			if err := ra.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
				t.Fatal(err)
			}
			apply(rb, absentRef, exhausted, false)
			current, err = rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rb.GetQuotaRecovery(ctx, v.ID); !errors.Is(err, repository.ErrNotFound) {
				t.Fatal("late result revived deleted row")
			}
			// Importing new material preserves both clocks and invalidates old material.
			before := current
			current.EncryptedAccessToken = "replacement"
			current.QuotaRecoveryRevision, current.QuotaRecoveryResetRevision = 0, 0
			replacement, _, err := ra.UpsertByIdentity(ctx, current)
			if err != nil {
				t.Fatal(err)
			}
			if replacement.QuotaRecoveryRevision != before.QuotaRecoveryRevision || replacement.QuotaRecoveryResetRevision != before.QuotaRecoveryResetRevision {
				t.Fatal("import reset quota clocks")
			}
			apply(rb, before.QuotaRecoveryRef(), exhausted, false)
			apply(rb, replacement.QuotaRecoveryRef(), exhausted, true)
			base, err := ra.ListRoutingAccountBases(ctx, v.Provider, "")
			if err != nil || len(base) != 1 || base[0].Credential.QuotaRecoveryRevision != replacement.QuotaRecoveryRevision+1 {
				t.Fatalf("routing lost recovery revision: %v", err)
			}
		})
	}
}

func TestQuotaRecoveryConcurrentFactsAndProbeClaims(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			v := newRecoveryAccount(t, ra, "concurrent")
			var notifications atomic.Int32
			observe := func(context.Context, repository.InvalidationEvent) { notifications.Add(1) }
			ra.SetInvalidationObserver(observe)
			rb.SetInvalidationObserver(observe)
			now := time.Now().UTC().Truncate(time.Microsecond)
			var wg sync.WaitGroup
			errs := make(chan error, 16)
			for i := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					result, err := repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: now.Add(-25*time.Hour + time.Duration(i)*time.Second), Used: int64(i + 1), Limit: 100})
					if err == nil && !result.Applied {
						err = errors.New("concurrent negative rejected")
					}
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			current, err := rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := rb.GetQuotaRecovery(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			expectedDue := now.Add(-time.Hour + 15*time.Second)
			if current.QuotaRecoveryRevision != 16 || recovery.ConfirmedUsed != 16 || recovery.NextProbeAt == nil || !recovery.NextProbeAt.Equal(expectedDue) || notifications.Load() != 16 {
				t.Fatalf("lost facts: rev=%d used=%d due=%v notifications=%d", current.QuotaRecoveryRevision, recovery.ConfirmedUsed, recovery.NextProbeAt, notifications.Load())
			}
			var claimed atomic.Int32
			errs = make(chan error, 16)
			for i := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					result, err := repo.ApplyQuotaRecovery(ctx, current.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryProbeClaimed, OccurredAt: now})
					if result.Applied {
						claimed.Add(1)
					}
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if claimed.Load() != 1 || notifications.Load() != 17 {
				t.Fatalf("claims=%d notifications=%d", claimed.Load(), notifications.Load())
			}
		})
	}
}

func TestQuotaRecoveryBillingAtomicRollbackAndOldResults(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, failTable := range []string{"account_billing_snapshots", "account_quota_recovery"} {
			t.Run(dialect+"/"+failTable, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ra, rb := NewAccountRepository(a), NewAccountRepository(b)
				ctx := context.Background()
				v := newRecoveryAccount(t, ra, "atomic")
				now := time.Now().UTC().Truncate(time.Microsecond)
				old := account.Billing{AccountID: v.ID, MonthlyLimit: 100, Used: 100, BillingPeriodEnd: now.Add(-time.Minute).Format(time.RFC3339), SyncedAt: now}
				seeded, err := ra.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryBillingObserved, Billing: &old, OccurredAt: now})
				if err != nil || !seeded.Applied {
					t.Fatal(err)
				}
				claimed, err := ra.ApplyQuotaRecovery(ctx, seeded.Ref, account.RecoveryEvent{Kind: account.RecoveryProbeClaimed, OccurredAt: now})
				if err != nil || !claimed.Applied {
					t.Fatal(err)
				}
				events := 0
				ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { events++ })
				injected := errors.New("injected recovery transaction failure")
				callback := func(tx *gorm.DB) {
					if tx.Statement.Table == failTable {
						tx.AddError(injected)
					}
				}
				// A still-exhausted Billing result updates both existing rows.
				if err := a.db.Callback().Update().Before("gorm:update").Register("recovery_atomic_failure", callback); err != nil {
					t.Fatal(err)
				}
				fresh := old
				fresh.Used = 200
				result, err := ra.ApplyQuotaRecovery(ctx, claimed.Ref, account.RecoveryEvent{Kind: account.RecoveryBillingObserved, Billing: &fresh, AfterProbe: true, OccurredAt: now})
				if cleanupErr := a.db.Callback().Update().Remove("recovery_atomic_failure"); cleanupErr != nil {
					t.Fatal(cleanupErr)
				}
				if !errors.Is(err, injected) || result.Applied || events != 0 {
					t.Fatalf("rollback applied=%v events=%d err=%v", result.Applied, events, err)
				}
				current, err := rb.Get(ctx, v.ID)
				if err != nil {
					t.Fatal(err)
				}
				billing, err := rb.GetBilling(ctx, v.ID)
				if err != nil {
					t.Fatal(err)
				}
				recovery, err := rb.GetQuotaRecovery(ctx, v.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.QuotaRecoveryRevision != claimed.Ref.Revision || billing.Used != old.Used || recovery.Status != account.QuotaRecoveryStatusProbing {
					t.Fatal("transaction partially committed")
				}
				fresh.Used = 0
				committed, err := ra.ApplyQuotaRecovery(ctx, claimed.Ref, account.RecoveryEvent{Kind: account.RecoveryBillingObserved, Billing: &fresh, AfterProbe: true, OccurredAt: now})
				if err != nil || !committed.Applied || !committed.Recovered || committed.Recovery != nil || events != 2 {
					t.Fatalf("retry %+v events=%d err=%v", committed, events, err)
				}
				for _, event := range []account.RecoveryEvent{{Kind: account.RecoveryPaidProbeFailed}, {Kind: account.RecoveryBillingObserved, Billing: &old, AfterProbe: true}, {Kind: account.RecoveryPaymentExhausted, Billing: &old}} {
					result, err := ra.ApplyQuotaRecovery(ctx, claimed.Ref, event)
					if err != nil || result.Applied || events != 2 {
						t.Fatalf("obsolete %s changed state: %+v %v", event.Kind, result, err)
					}
				}
				got, err := rb.GetBilling(ctx, v.ID)
				if err != nil || got.Used != 0 {
					t.Fatalf("old observation overwrote billing: %v", err)
				}
			})
		}
	}
}

func TestQuotaRecoveryResetOverflowRollsBackWholeScope(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, scope := range []string{"selected", "all"} {
			t.Run(dialect+"/"+scope, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ra, rb := NewAccountRepository(a), NewAccountRepository(b)
				ctx := context.Background()
				first := seedQuotaResetAccount(t, ra, account.ProviderBuild, "normal")
				full := seedQuotaResetAccount(t, ra, account.ProviderBuild, "full")
				if err := a.db.Model(&accountModel{}).Where("id = ?", full.ID).UpdateColumn("quota_recovery_revision", int64(math.MaxInt64)).Error; err != nil {
					t.Fatal(err)
				}
				current, err := rb.Get(ctx, first.ID)
				if err != nil {
					t.Fatal(err)
				}
				events := 0
				ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { events++ })
				if scope == "selected" {
					err = ra.ResetQuotaState(ctx, first.Provider, []uint64{first.ID, full.ID})
				} else {
					_, err = ra.ResetProviderQuotaState(ctx, first.Provider, true)
				}
				if !errors.Is(err, account.ErrQuotaRecoveryRevisionExhausted) || events != 0 {
					t.Fatalf("reset overflow events=%d err=%v", events, err)
				}
				after, err := rb.Get(ctx, first.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.QuotaRecoveryRevision != current.QuotaRecoveryRevision {
					t.Fatal("partial reset clock commit")
				}
				assertQuotaResetTables(t, b, first.ID, false)
				assertQuotaResetTables(t, b, full.ID, false)
				maxed, err := rb.Get(ctx, full.ID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = ra.ApplyQuotaRecovery(ctx, maxed.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted})
				if !errors.Is(err, account.ErrQuotaRecoveryRevisionExhausted) {
					t.Fatalf("event overflow: %v", err)
				}
			})
		}
	}
}

func TestQuotaRecoveryLegacyClockMigration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra := NewAccountRepository(a)
			ctx := context.Background()
			v := seedQuotaResetAccount(t, ra, account.ProviderBuild, "legacy")
			drop := func(tx *gorm.DB) error {
				for _, name := range []string{"chk_accounts_recovery_reset", "chk_accounts_recovery_revision"} {
					if err := tx.Migrator().DropConstraint(&accountModel{}, name); err != nil {
						return err
					}
				}
				for _, name := range []string{"quota_recovery_reset_revision", "quota_recovery_revision"} {
					if err := tx.Migrator().DropColumn(&accountModel{}, name); err != nil {
						return err
					}
				}
				return nil
			}
			var err error
			if dialect == "sqlite" {
				err = a.withSQLiteForeignKeysDisabled(ctx, func() error { return drop(a.db) })
			} else {
				err = drop(a.db)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			rb := NewAccountRepository(b)
			current, err := rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.QuotaRecoveryRevision != 0 || current.QuotaRecoveryResetRevision != 0 {
				t.Fatal("legacy clock is not zero")
			}
			assertQuotaResetTables(t, b, v.ID, false)
			result, err := rb.ApplyQuotaRecovery(ctx, current.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, Used: 77})
			if err != nil || !result.Applied || result.Ref.Revision != 1 {
				t.Fatalf("legacy adoption: %+v %v", result, err)
			}
			// Wrong providers cannot reuse a valid account ID as another recovery scope.
			wrong := current.QuotaRecoveryRef()
			wrong.Provider = account.ProviderWeb
			if _, err := rb.ApplyQuotaRecovery(ctx, wrong, account.RecoveryEvent{Kind: account.RecoveryFreeExhausted}); !errors.Is(err, repository.ErrNotFound) {
				t.Fatal(fmt.Sprintf("wrong provider: %v", err))
			}
		})
	}
}
