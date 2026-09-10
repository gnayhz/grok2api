package relational

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// ApplyModelRestriction serializes observed results with credential replacement
// and quota reset. It changes only the matching model/reason, then invalidates
// the overlay after commit. Obsolete and shorter observations are no-ops.
func (r *AccountRepository) ApplyModelRestriction(ctx context.Context, ref account.QuotaRecoveryRef, event account.ModelRestrictionEvent) (account.ModelRestrictionResult, error) {
	var result account.ModelRestrictionResult
	if ref.AccountID == 0 {
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
		var stored accountModelQuotaBlockModel
		var block *account.ModelQuotaBlock
		err := tx.First(&stored, "account_id = ? AND upstream_model = ? AND reason = ?", ref.AccountID, strings.TrimSpace(event.UpstreamModel), event.Kind).Error
		if err == nil {
			v := modelRestrictionDomain(stored)
			block = &v
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := event.OccurredAt
		if now.IsZero() {
			now = time.Now().UTC()
		}
		result, err = account.TransitionModelRestriction(toAccountDomain(row), block, ref, event, now)
		if err != nil || !result.Applied {
			return err
		}
		next := accountModelQuotaBlockModel{AccountID: ref.AccountID, UpstreamModel: result.Block.UpstreamModel, Reason: result.Block.Reason, CooldownUntil: result.Block.CooldownUntil, UpdatedAt: result.Block.UpdatedAt}
		return tx.Save(&next).Error
	})
	if err != nil {
		return account.ModelRestrictionResult{}, mapError(err)
	}
	if result.Applied {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged, Provider: ref.Provider, AccountID: ref.AccountID, UpstreamModel: result.Block.UpstreamModel})
	}
	return result, nil
}

func modelRestrictionDomain(row accountModelQuotaBlockModel) account.ModelQuotaBlock {
	return account.ModelQuotaBlock{AccountID: row.AccountID, UpstreamModel: row.UpstreamModel, Reason: row.Reason, CooldownUntil: row.CooldownUntil.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
}
