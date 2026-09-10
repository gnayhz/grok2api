package relational

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"gorm.io/gorm"
)

func TestQuotaResetUsesOneActiveCohort(t *testing.T) {
	// PostgreSQL administration must serialize after the reset account lock;
	// both child deletes still use the same active cohort.
	a, b := settingsDatabasePair(t, "postgres")
	ra, rb := NewAccountRepository(a), NewAccountRepository(b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "cohort", SourceKey: "cohort", EncryptedAccessToken: "token", Enabled: true, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	if err := testsupport.Recovery(ctx, ra, account.QuotaRecovery{AccountID: v.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted, NextProbeAt: &until}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Create(&accountModelQuotaBlockModel{AccountID: v.ID, UpstreamModel: "quota-model", Reason: "model_quota_depleted", CooldownUntil: until}).Error; err != nil {
		t.Fatal(err)
	}
	adminDone := make(chan error, 1)
	if err := a.db.Callback().Delete().After("gorm:delete").Register("quota_reset_change_cohort", func(tx *gorm.DB) {
		if tx.Statement.Table != "account_quota_recovery" || tx.Error != nil {
			return
		}
		go func() {
			disabled := false
			_, err := rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &disabled}})
			adminDone <- err
		}()
		select {
		case err := <-adminDone:
			tx.AddError(fmt.Errorf("administration crossed reset lock: %v", err))
		case <-time.After(30 * time.Millisecond):
		}

	}); err != nil {
		t.Fatal(err)
	}
	defer a.db.Callback().Delete().Remove("quota_reset_change_cohort")
	n, err := ra.ResetProviderQuotaState(ctx, v.Provider, true)
	if err != nil || n != 1 {
		t.Fatalf("reset count=%d err=%v", n, err)
	}
	if err := <-adminDone; err != nil {
		t.Fatal(err)
	}
	var recovery, blocks int64
	if err := b.db.Model(&quotaRecoveryModel{}).Where("account_id = ?", v.ID).Count(&recovery).Error; err != nil {
		t.Fatal(err)
	}
	if err := b.db.Model(&accountModelQuotaBlockModel{}).Where("account_id = ?", v.ID).Count(&blocks).Error; err != nil {
		t.Fatal(err)
	}
	if recovery != 0 || blocks != 0 {
		t.Fatalf("one reset used different account sets: recovery=%d quota blocks=%d", recovery, blocks)
	}
	current, err := rb.Get(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Enabled {
		t.Fatal("reset overrode concurrent disable")
	}
}

func TestQuotaResetConcurrentQuotaWriteRollsBackSnapshot(t *testing.T) {
	a, b := settingsDatabasePair(t, "postgres")
	ra := NewAccountRepository(a)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := seedQuotaResetAccount(t, ra, account.ProviderBuild, "snapshot-conflict")
	events := 0
	ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { events++ })
	until := time.Now().UTC().Truncate(time.Microsecond).Add(3 * time.Hour)
	if err := a.db.Callback().Delete().Before("gorm:delete").Register("quota_reset_concurrent_block", func(tx *gorm.DB) {
		if tx.Statement.Table != "account_quota_recovery" || tx.Error != nil {
			return
		}
		// Inject a child-row write outside the new account event gate to test
		// PostgreSQL snapshot rollback itself. Production observations now
		// lock the account first and are covered by the event/reset test.
		if err := b.db.WithContext(ctx).Model(&accountModelQuotaBlockModel{}).Where("account_id = ? AND upstream_model = ? AND reason = ?", v.ID, "quota-model", "model_quota_depleted").UpdateColumn("cooldown_until", until).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := ra.ResetProviderQuotaState(ctx, v.Provider, true)
	if cleanupErr := a.db.Callback().Delete().Remove("quota_reset_concurrent_block"); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	var sqlErr interface{ SQLState() string }
	if !errors.As(err, &sqlErr) || sqlErr.SQLState() != "40001" {
		t.Fatalf("expected snapshot conflict, got %v", err)
	}
	if events != 0 {
		t.Fatal("failed reset published success")
	}
	assertQuotaResetTables(t, b, v.ID, false)
	var block accountModelQuotaBlockModel
	if err := b.db.Where("account_id = ? AND upstream_model = ?", v.ID, "quota-model").Take(&block).Error; err != nil {
		t.Fatal(err)
	}
	if !block.CooldownUntil.Equal(until) {
		t.Fatal("failed reset lost committed concurrent quota fact")
	}
	n, err := ra.ResetProviderQuotaState(ctx, v.Provider, true)
	if err != nil || n != 1 {
		t.Fatalf("retry count=%d err=%v", n, err)
	}
	assertQuotaResetTables(t, b, v.ID, true)
}
