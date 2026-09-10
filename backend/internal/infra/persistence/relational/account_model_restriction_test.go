package relational

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestModelRestrictionConcurrentReasonsAndRoutingProjection(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			v := newRecoveryAccount(t, ra, "concurrent-model")
			now := time.Now().UTC().Truncate(time.Microsecond)
			var notices atomic.Int32
			observer := func(_ context.Context, e repository.InvalidationEvent) {
				if e.Kind != repository.InvalidationAccountModelQuotaChanged || e.UpstreamModel != "model" || e.AccountID != v.ID {
					t.Errorf("unexpected model invalidation: %+v", e)
				}
				notices.Add(1)
			}
			ra.SetInvalidationObserver(observer)
			rb.SetInvalidationObserver(observer)
			var wg sync.WaitGroup
			failures := make(chan error, 16)
			for i := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					kind := account.ModelAccessDenied
					if i%2 == 0 {
						kind = account.ModelQuotaExhausted
					}
					_, err := repo.ApplyModelRestriction(ctx, v.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: kind, UpstreamModel: "model", OccurredAt: now, RetryAfter: time.Duration(i+1) * time.Hour})
					failures <- err
				}()
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			var rows []accountModelQuotaBlockModel
			if err := b.db.Where("account_id = ?", v.ID).Order("reason").Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0].Reason != string(account.ModelAccessDenied) || !rows[0].CooldownUntil.Equal(now.Add(16*time.Hour)) || rows[1].Reason != string(account.ModelQuotaExhausted) || !rows[1].CooldownUntil.Equal(now.Add(15*time.Hour)) {
				t.Fatalf("concurrent reasons=%+v", rows)
			}
			if notices.Load() < 2 || notices.Load() > 16 {
				t.Fatalf("notices=%d", notices.Load())
			}
			current, err := rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.CredentialRef() != v.CredentialRef() || current.QuotaRecoveryRef() != v.QuotaRecoveryRef() || current.HealthRevision != v.HealthRevision || !current.UpdatedAt.Equal(v.UpdatedAt) {
				t.Fatal("model facts changed credential/health/recovery clocks")
			}
			assertProjection := func(reason string, until time.Time) {
				t.Helper()
				combined, err := ra.ListRoutingCandidates(ctx, v.Provider, 0, "model", "")
				if err != nil || len(combined) != 1 {
					t.Fatalf("combined=%d err=%v", len(combined), err)
				}
				overlays, err := rb.ListRoutingAccountOverlays(ctx, v.Provider, 0, "model")
				if err != nil || len(overlays.Values) != 1 {
					t.Fatalf("overlays=%d err=%v", len(overlays.Values), err)
				}
				for _, block := range []*account.ModelQuotaBlock{combined[0].ModelQuotaBlock, overlays.Values[0].ModelQuotaBlock} {
					if block == nil || block.Reason != reason || !block.CooldownUntil.Equal(until) {
						t.Fatalf("projection=%+v", block)
					}
				}
			}
			assertProjection(string(account.ModelAccessDenied), now.Add(16*time.Hour))
			if _, err := rb.ApplyModelRestriction(ctx, v.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: account.ModelQuotaExhausted, UpstreamModel: "model", OccurredAt: now, RetryAfter: 20 * time.Hour}); err != nil {
				t.Fatal(err)
			}
			assertProjection(string(account.ModelQuotaExhausted), now.Add(20*time.Hour))
			ra.SetInvalidationObserver(nil)
			rb.SetInvalidationObserver(nil)
			if err := ra.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
				t.Fatal(err)
			}
			assertProjection(string(account.ModelAccessDenied), now.Add(16*time.Hour))
		})
	}
}

func TestModelRestrictionResetAndMaterialFencesAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, change := range []string{"selected_reset", "active_reset", "material"} {
			for _, kind := range []account.ModelRestrictionKind{account.ModelQuotaExhausted, account.ModelAccessDenied} {
				t.Run(fmt.Sprintf("%s/%s/%s", dialect, change, kind), func(t *testing.T) {
					a, b := settingsDatabasePair(t, dialect)
					ra, rb := NewAccountRepository(a), NewAccountRepository(b)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					v := newRecoveryAccount(t, ra, "fenced-model")
					entered, resume := make(chan struct{}), make(chan struct{})
					var once sync.Once
					defer once.Do(func() { close(resume) })
					// The upstream result retains the material observed before
					// IO and reaches its write gate after the other connection
					// commits. No transaction is held while waiting on IO.
					type outcome struct {
						result account.ModelRestrictionResult
						err    error
					}
					done := make(chan outcome, 1)
					go func() {
						close(entered)
						select {
						case <-resume:
						case <-ctx.Done():
							done <- outcome{err: ctx.Err()}
							return
						}
						r, e := rb.ApplyModelRestriction(ctx, v.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: kind, UpstreamModel: "model"})
						done <- outcome{r, e}
					}()
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					var err error
					switch change {
					case "selected_reset":
						err = ra.ResetQuotaState(ctx, v.Provider, []uint64{v.ID})
					case "active_reset":
						_, err = ra.ResetProviderQuotaState(ctx, v.Provider, true)
					case "material":
						replacement := v
						replacement.EncryptedAccessToken = "new-material"
						_, _, err = ra.UpsertByIdentity(ctx, replacement)
					}
					if err != nil {
						t.Fatal(err)
					}
					once.Do(func() { close(resume) })
					var got outcome
					select {
					case got = <-done:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					want := change != "material" && kind == account.ModelAccessDenied
					if got.err != nil || got.result.Applied != want {
						t.Fatalf("late event applied=%v want=%v err=%v", got.result.Applied, want, got.err)
					}
					var n int64
					if err := a.db.Model(&accountModelQuotaBlockModel{}).Where("account_id = ?", v.ID).Count(&n).Error; err != nil {
						t.Fatal(err)
					}
					expected := int64(0)
					if want {
						expected = 1
					}
					if n != expected {
						t.Fatalf("persisted rows=%d want=%d", n, expected)
					}
					current, err := ra.Get(ctx, v.ID)
					if err != nil {
						t.Fatal(err)
					}
					result, err := rb.ApplyModelRestriction(ctx, current.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: kind, UpstreamModel: "new-model"})
					if err != nil || !result.Applied {
						t.Fatalf("current result rejected: %+v %v", result, err)
					}
				})
			}
		}
	}
}

func TestModelRestrictionPruneKeysAndConcurrentExtension(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v := newRecoveryAccount(t, ra, "prune-model")
			now := time.Now().UTC().Truncate(time.Microsecond)
			for _, kind := range []account.ModelRestrictionKind{account.ModelQuotaExhausted, account.ModelAccessDenied} {
				result, err := ra.ApplyModelRestriction(ctx, v.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: kind, UpstreamModel: "model", RetryAfter: time.Minute, OccurredAt: now.Add(-2 * time.Minute)})
				if err != nil || !result.Applied {
					t.Fatalf("seed=%+v %v", result, err)
				}
			}
			n, err := rb.PruneExpiredModelQuotaBlocks(ctx, now, 1)
			if err != nil || n != 1 {
				t.Fatalf("limited prune rows=%d err=%v", n, err)
			}
			var remaining accountModelQuotaBlockModel
			if err := a.db.Where("account_id = ?", v.ID).Take(&remaining).Error; err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			if err := a.db.Callback().Query().After("gorm:query").Register("extend_after_prune_scan", func(tx *gorm.DB) {
				if tx.Statement.Table == "account_model_quota_blocks" {
					once.Do(func() {
						_, err := rb.ApplyModelRestriction(ctx, v.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: account.ModelRestrictionKind(remaining.Reason), UpstreamModel: "model", RetryAfter: time.Hour, OccurredAt: now})
						if err != nil {
							tx.AddError(err)
						}
					})
				}
			}); err != nil {
				t.Fatal(err)
			}
			n, err = ra.PruneExpiredModelQuotaBlocks(ctx, now, 1)
			if cleanup := a.db.Callback().Query().Remove("extend_after_prune_scan"); cleanup != nil {
				t.Fatal(cleanup)
			}
			if err != nil || n != 0 {
				t.Fatalf("prune erased extended fact: n=%d err=%v", n, err)
			}
			if err := a.db.Where("account_id = ?", v.ID).Take(&remaining).Error; err != nil || !remaining.CooldownUntil.Equal(now.Add(time.Hour)) {
				t.Fatalf("surviving fact=%+v err=%v", remaining, err)
			}
		})
	}
}

func TestModelRestrictionCommitSerializesBeforeReset(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []account.ModelRestrictionKind{account.ModelQuotaExhausted, account.ModelAccessDenied} {
			t.Run(dialect+"/"+string(kind), func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ra, rb := NewAccountRepository(a), NewAccountRepository(b)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				v := newRecoveryAccount(t, ra, "locked-model")
				entered, resume := make(chan struct{}), make(chan struct{})
				var release sync.Once
				defer release.Do(func() { close(resume) })
				var intercept sync.Once
				if err := a.db.Callback().Query().After("gorm:query").Register("model_holds_account_lock", func(tx *gorm.DB) {
					if tx.Statement.Table == "provider_accounts" {
						intercept.Do(func() {
							close(entered)
							select {
							case <-resume:
							case <-ctx.Done():
								tx.AddError(ctx.Err())
							}
						})
					}
				}); err != nil {
					t.Fatal(err)
				}
				eventDone := make(chan error, 1)
				go func() {
					result, err := ra.ApplyModelRestriction(ctx, v.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: kind, UpstreamModel: "model"})
					if err == nil && !result.Applied {
						err = fmt.Errorf("current event rejected")
					}
					eventDone <- err
				}()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				resetDone := make(chan error, 1)
				go func() { resetDone <- rb.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}) }()
				select {
				case err := <-resetDone:
					t.Fatalf("reset passed active model transaction: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
				release.Do(func() { close(resume) })
				if err := <-eventDone; err != nil {
					t.Fatal(err)
				}
				if err := a.db.Callback().Query().Remove("model_holds_account_lock"); err != nil {
					t.Fatal(err)
				}
				if err := <-resetDone; err != nil {
					t.Fatal(err)
				}
				var n int64
				if err := a.db.Model(&accountModelQuotaBlockModel{}).Where("account_id = ?", v.ID).Count(&n).Error; err != nil {
					t.Fatal(err)
				}
				want := int64(0)
				if kind == account.ModelAccessDenied {
					want = 1
				}
				if n != want {
					t.Fatalf("serialized reset rows=%d want=%d", n, want)
				}
			})
		}
	}
}
