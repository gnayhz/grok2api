package relational

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// ApplyQuotaRecovery serializes claims, observations and resets on the account
// row. Billing and recovery are committed together before invalidation is sent.
func (r *AccountRepository) ApplyQuotaRecovery(ctx context.Context, ref account.QuotaRecoveryRef, event account.RecoveryEvent) (account.RecoveryResult, error) {
	var result account.RecoveryResult
	if ref.AccountID == 0 || ref.Provider != account.ProviderBuild {
		return result, repository.ErrNotFound
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockProviderAccount(tx, ref.AccountID, ref.Provider); err != nil {
			return err
		}
		var row accountModel
		if err := tx.Preload("Credential").First(&row, ref.AccountID).Error; err != nil {
			return err
		}
		if row.Credential == nil {
			return repository.ErrNotFound
		}
		var stored quotaRecoveryModel
		var recovery *account.QuotaRecovery
		err := tx.First(&stored, "account_id = ?", ref.AccountID).Error
		if err == nil {
			v := quotaRecoveryDomain(stored)
			recovery = &v
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := event.OccurredAt
		if now.IsZero() {
			now = time.Now().UTC()
		}
		result, err = account.TransitionQuotaRecovery(toAccountDomain(row), recovery, ref, event, now)
		if err != nil || !result.Applied {
			return err
		}
		if err := tx.Model(&accountModel{}).Where("id = ?", ref.AccountID).UpdateColumns(map[string]any{
			"quota_recovery_revision": result.Ref.Revision, "quota_recovery_reset_revision": result.ResetRevision,
		}).Error; err != nil {
			return err
		}
		if event.Kind == account.RecoveryBillingObserved {
			if err := saveBilling(tx, *event.Billing); err != nil {
				return err
			}
		}
		if result.Recovery == nil {
			return tx.Where("account_id = ?", ref.AccountID).Delete(&quotaRecoveryModel{}).Error
		}
		next := quotaRecoveryRow(*result.Recovery)
		return tx.Save(&next).Error
	})
	if err != nil {
		return account.RecoveryResult{}, mapError(err)
	}
	if result.Applied {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, Provider: ref.Provider, AccountID: ref.AccountID})
		if event.Kind == account.RecoveryBillingObserved {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountBillingChanged, Provider: ref.Provider, AccountID: ref.AccountID})
		}
	}
	return result, nil
}

func quotaRecoveryDomain(row quotaRecoveryModel) account.QuotaRecovery {
	return account.QuotaRecovery{AccountID: row.AccountID, Kind: account.QuotaRecoveryKind(row.Kind), Status: account.QuotaRecoveryStatus(row.Status), ConfirmedUsed: row.ConfirmedUsed, ConfirmedLimit: row.ConfirmedLimit, ExhaustedAt: row.ExhaustedAt, NextProbeAt: row.NextProbeAt, LastConfirmedAt: row.LastConfirmedAt, UpdatedAt: row.UpdatedAt}
}
func quotaRecoveryRow(value account.QuotaRecovery) quotaRecoveryModel {
	return quotaRecoveryModel{AccountID: value.AccountID, Kind: string(value.Kind), Status: string(value.Status), ConfirmedUsed: value.ConfirmedUsed, ConfirmedLimit: value.ConfirmedLimit, ExhaustedAt: value.ExhaustedAt, NextProbeAt: value.NextProbeAt, LastConfirmedAt: value.LastConfirmedAt, UpdatedAt: value.UpdatedAt}
}

// The conditional update locks the selected rows and detects exhaustion without
// integer overflow. Its transaction rolls back every row if any clock is full.
func advanceQuotaReset(query func() *gorm.DB) error {
	var count int64
	if err := query().Count(&count).Error; err != nil {
		return err
	}
	result := query().Where("quota_recovery_revision < ?", int64(math.MaxInt64)).UpdateColumns(map[string]any{
		"quota_recovery_revision":       gorm.Expr("quota_recovery_revision + 1"),
		"quota_recovery_reset_revision": gorm.Expr("quota_recovery_revision + 1"),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != count {
		return account.ErrQuotaRecoveryRevisionExhausted
	}
	return nil
}
