package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// Patch serializes explicit management fields on the key row. A rename never
// rewrites grants, scope, availability, or billing facts from an old snapshot.
func (r *ClientKeyRepository) Patch(ctx context.Context, id uint64, input clientkey.ManagementPatch) (clientkey.Key, error) {
	patch, err := input.Normalize()
	if err != nil {
		return clientkey.Key{}, repository.ErrInvalidRecord
	}
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if patch.Name != nil {
		updates["name"] = *patch.Name
	}
	if patch.Enabled != nil {
		updates["enabled"] = *patch.Enabled
	}
	if patch.ClearExpiresAt {
		updates["expires_at"] = nil
	} else if patch.ExpiresAt != nil {
		updates["expires_at"] = patch.ExpiresAt
	}
	if patch.RPMLimit != nil {
		updates["rpm_limit"] = *patch.RPMLimit
	}
	if patch.MaxConcurrent != nil {
		updates["max_concurrent"] = *patch.MaxConcurrent
	}
	if patch.BillingLimitUSDTicks != nil {
		updates["billing_limit_usd_ticks"] = *patch.BillingLimitUSDTicks
	}
	if patch.AllowModelAliases != nil {
		updates["allow_model_aliases"] = *patch.AllowModelAliases
	}
	if patch.ProviderScope != nil {
		updates["provider_scope_mask"] = uint8(*patch.ProviderScope)
	}
	if patch.TierScope != nil {
		updates["tier_scope_mask"] = uint8(*patch.TierScope)
	}
	if patch.ModelScope != nil {
		updates["model_scope"] = string(*patch.ModelScope)
	}
	var value clientkey.Key
	err = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&clientKeyModel{}).Where("id = ? AND internal_kind IS NULL", id).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		if patch.AllowedModels != nil {
			if err := replacePermissions(tx, id, *patch.AllowedModels); err != nil {
				return err
			}
		}
		var err error
		value, err = readClientKey(tx.Where("id = ?", id))
		return err
	})
	if err != nil {
		return clientkey.Key{}, mapError(err)
	}
	r.notifyInvalidation(ctx, id)
	return value, nil
}
