package relational

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"gorm.io/gorm"
)

func seedQuotaResetAccount(t *testing.T, r *AccountRepository, provider account.Provider, key string) account.Credential {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	until := now.Add(time.Hour)
	v, _, err := r.UpsertByIdentity(ctx, account.Credential{Provider: provider, Name: key, SourceKey: key, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", Enabled: true, AuthStatus: account.AuthStatusActive, FailureCount: 3, LastError: account.LastErrorQualityIdle, CooldownUntil: &until, RiskStatus: account.RiskStatusRSCDenied})
	if err != nil {
		t.Fatal(err)
	}
	recovery := account.QuotaRecovery{AccountID: v.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted, NextProbeAt: &until, ExhaustedAt: &now, UpdatedAt: now}
	if provider == account.ProviderBuild {
		if err := testsupport.Recovery(ctx, r, recovery); err != nil {
			t.Fatal(err)
		}
	} else {
		// Legacy cross-provider rows must remain scoped correctly by reset.
		row := quotaRecoveryRow(recovery)
		if err := r.db.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for model, reason := range map[string]string{"quota-model": "model_quota_depleted", "denied-model": "model_access_denied"} {
		if err := testsupport.ModelRestriction(ctx, r, account.ModelQuotaBlock{AccountID: v.ID, UpstreamModel: model, Reason: reason, CooldownUntil: until}); err != nil {
			t.Fatal(err)
		}
	}
	billing := account.Billing{AccountID: v.ID, MonthlyLimit: 100, Used: 42, SyncedAt: now}
	if provider == account.ProviderBuild {
		if err := testsupport.Billing(ctx, r, billing); err != nil {
			t.Fatal(err)
		}
	} else if err := saveBilling(r.db.db, billing); err != nil {
		t.Fatal(err)
	}
	return v
}

func assertQuotaResetTables(t *testing.T, db *Database, id uint64, cleared bool) {
	t.Helper()
	want := int64(1)
	if cleared {
		want = 0
	}
	for _, table := range []string{"account_quota_recovery", "account_model_quota_blocks"} {
		query := db.db.Table(table).Where("account_id = ?", id)
		if table == "account_model_quota_blocks" {
			query = query.Where("reason = ?", "model_quota_depleted")
		}
		var n int64
		if err := query.Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("account %d table %s rows=%d want=%d", id, table, n, want)
		}
	}
	var denied int64
	if err := db.db.Model(&accountModelQuotaBlockModel{}).Where("account_id = ? AND reason = ?", id, "model_access_denied").Count(&denied).Error; err != nil {
		t.Fatal(err)
	}
	if denied != 1 {
		t.Errorf("account %d lost model access restriction", id)
	}
	billing, err := NewAccountRepository(db).GetBilling(context.Background(), id)
	if err != nil || billing.Used != 42 {
		t.Fatalf("billing changed: err=%v", err)
	}
}

func TestQuotaResetScopesAndIndependentStateAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, scope := range []string{"selected", "active", "provider"} {
			t.Run(dialect+"/"+scope, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ra, rb := NewAccountRepository(a), NewAccountRepository(b)
				ctx := context.Background()
				active := seedQuotaResetAccount(t, ra, account.ProviderBuild, "active")
				disabled := seedQuotaResetAccount(t, ra, account.ProviderBuild, "disabled")
				rejected := seedQuotaResetAccount(t, ra, account.ProviderBuild, "rejected")
				web := seedQuotaResetAccount(t, ra, account.ProviderWeb, "web")
				enabled := false
				if _, err := rb.UpdateAdministration(ctx, disabled.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &enabled}}); err != nil {
					t.Fatal(err)
				}
				if _, err := rb.ApplyCredential(ctx, rejected.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRejected, Reason: "auth-restriction"}); err != nil {
					t.Fatal(err)
				}
				rows := []account.Credential{active, disabled, rejected, web}
				before := make([]account.Credential, len(rows))
				for i, v := range rows {
					current, err := rb.Get(ctx, v.ID)
					if err != nil {
						t.Fatal(err)
					}
					before[i] = current
				}
				var events []repository.InvalidationEvent
				ra.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { events = append(events, e) })
				switch scope {
				case "selected":
					if err := ra.ResetQuotaState(ctx, account.ProviderBuild, []uint64{active.ID, disabled.ID, web.ID}); err != nil {
						t.Fatal(err)
					}
				default:
					n, err := ra.ResetProviderQuotaState(ctx, account.ProviderBuild, scope == "active")
					want := int64(3)
					if scope == "active" {
						want = 1
					}
					if err != nil || n != want {
						t.Fatalf("count=%d want=%d err=%v", n, want, err)
					}
				}
				if len(events) != 2 || events[0].Kind != repository.InvalidationAccountRecoveryChanged || events[1].Kind != repository.InvalidationAccountModelQuotaChanged || events[0].Provider != account.ProviderBuild || events[1].Provider != account.ProviderBuild {
					t.Fatalf("reset notifications: %+v", events)
				}
				for i, v := range rows {
					cleared := v.ID == active.ID || (scope != "active" && v.ID == disabled.ID) || (scope == "provider" && v.ID == rejected.ID)
					assertQuotaResetTables(t, b, v.ID, cleared)
					after, err := rb.Get(ctx, v.ID)
					if err != nil {
						t.Fatal(err)
					}
					if cleared {
						if after.QuotaRecoveryRevision != before[i].QuotaRecoveryRevision+1 || after.QuotaRecoveryResetRevision != after.QuotaRecoveryRevision {
							t.Fatal("reset clock did not advance")
						}
						before[i].QuotaRecoveryRevision, before[i].QuotaRecoveryResetRevision = after.QuotaRecoveryRevision, after.QuotaRecoveryResetRevision
					}
					if !reflect.DeepEqual(before[i], after) {
						t.Errorf("reset changed credential/admin/health/auth/risk fields of %d", v.ID)
					}
				}
			})
		}
	}
}

func TestQuotaResetRollsBackBothTablesAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, all := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/all=%t", dialect, all), func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ra := NewAccountRepository(a)
				ctx := context.Background()
				v := seedQuotaResetAccount(t, ra, account.ProviderBuild, "rollback")
				var notifications atomic.Int32
				ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { notifications.Add(1) })
				injected := errors.New("model block delete unavailable")
				if err := a.db.Callback().Delete().Before("gorm:delete").Register("quota_reset_delete_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "account_model_quota_blocks" {
						tx.AddError(injected)
					}
				}); err != nil {
					t.Fatal(err)
				}
				reset := func() error {
					if all {
						_, err := ra.ResetProviderQuotaState(ctx, v.Provider, true)
						return err
					}
					return ra.ResetQuotaState(ctx, v.Provider, []uint64{v.ID})
				}
				err := reset()
				if err := a.db.Callback().Delete().Remove("quota_reset_delete_failure"); err != nil {
					t.Fatal(err)
				}
				if !errors.Is(err, injected) {
					t.Fatalf("failure not returned: %v", err)
				}
				if notifications.Load() != 0 {
					t.Fatal("rollback published success")
				}
				assertQuotaResetTables(t, b, v.ID, false)
				if err := reset(); err != nil {
					t.Fatal(err)
				}
				assertQuotaResetTables(t, b, v.ID, true)
				if notifications.Load() != 2 {
					t.Fatalf("notifications=%d", notifications.Load())
				}
			})
		}
	}
}

func TestQuotaResetConcurrentHealthAndAdministration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v := seedQuotaResetAccount(t, ra, account.ProviderBuild, "concurrent")
			var wg sync.WaitGroup
			errs := make(chan error, 24)
			var conflicts atomic.Int32
			enabled := false
			name := "edited"
			for i := range 24 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var err error
					switch i % 4 {
					case 0:
						_, err = ra.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthFailure, Status: 503, CooldownBase: time.Hour, CooldownMax: time.Hour})
					case 1:
						_, err = rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{Name: &name, AccountUpdates: repository.AccountUpdates{Enabled: &enabled}})
					case 2:
						err = ra.ResetQuotaState(ctx, v.Provider, []uint64{v.ID})
					case 3:
						_, err = rb.ResetProviderQuotaState(ctx, v.Provider, true)
						// Repeatable Read may reject a reset when another reset
						// already deleted its snapshot's rows. The separate forced
						// conflict test proves full rollback and a successful retry.
						var sqlErr interface{ SQLState() string }
						if dialect == "postgres" && errors.As(err, &sqlErr) && sqlErr.SQLState() == "40001" {
							conflicts.Add(1)
							err = nil
						}
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
			t.Logf("concurrent resets rejected with snapshot conflict: %d", conflicts.Load())
			after, err := ra.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Enabled || after.Name != name || after.FailureCount != 9 || after.HealthRevision != 6 || after.CooldownUntil == nil || after.RiskStatus != v.RiskStatus {
				t.Fatalf("parallel reset lost another owner: enabled=%t failures=%d revision=%d", after.Enabled, after.FailureCount, after.HealthRevision)
			}
			assertQuotaResetTables(t, b, v.ID, true)
		})
	}
}

func TestModelRestrictionExtensionAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v := seedQuotaResetAccount(t, ra, account.ProviderBuild, "block")
			until := time.Now().UTC().Truncate(time.Microsecond).Add(2 * time.Hour)
			long := account.ModelQuotaBlock{AccountID: v.ID, UpstreamModel: "denied-model", Reason: "model_access_denied", CooldownUntil: until}
			if err := testsupport.ModelRestriction(ctx, ra, long); err != nil {
				t.Fatal(err)
			}
			short := long
			short.Reason = "model_quota_depleted"
			short.CooldownUntil = until.Add(-time.Hour)
			if err := testsupport.ModelRestriction(ctx, rb, short); err != nil {
				t.Fatal(err)
			}
			var row accountModelQuotaBlockModel
			if err := a.db.Where("account_id = ? AND upstream_model = ? AND reason = ?", v.ID, long.UpstreamModel, long.Reason).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.Reason != long.Reason || !row.CooldownUntil.Equal(long.CooldownUntil) {
				t.Fatal("shorter late block replaced current restriction")
			}
			var rows int64
			if err := b.db.Model(&accountModelQuotaBlockModel{}).Where("account_id = ? AND upstream_model = ?", v.ID, long.UpstreamModel).Count(&rows).Error; err != nil || rows != 2 {
				t.Fatalf("independent same-model reasons=%d err=%v", rows, err)
			}
			shorterSameReason := long
			shorterSameReason.CooldownUntil = until.Add(-time.Minute)
			if err := testsupport.ModelRestriction(ctx, rb, shorterSameReason); err != nil {
				t.Fatal(err)
			}
			long.CooldownUntil = until.Add(time.Hour)
			if err := testsupport.ModelRestriction(ctx, rb, long); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Where("account_id = ? AND upstream_model = ? AND reason = ?", v.ID, long.UpstreamModel, long.Reason).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if !row.CooldownUntil.Equal(long.CooldownUntil) {
				t.Fatal("longer restriction was not extended")
			}
		})
	}
}
