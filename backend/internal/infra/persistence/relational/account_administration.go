package relational

import (
	"context"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// UpdateAdministration commits the submitted fields, including Cookie material
// and manual risk attribution, together. It never saves a caller's account snapshot.
func (r *AccountRepository) UpdateAdministration(ctx context.Context, id uint64, patch repository.AccountAdminPatch) (repository.AccountAdminResult, error) {
	var result repository.AccountAdminResult
	var provider account.Provider
	changed := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockProviderAccount(tx, id, ""); err != nil {
			return err
		}
		var current accountModel
		if err := tx.First(&current, id).Error; err != nil {
			return err
		}
		provider = account.Provider(current.Provider)
		if provider != account.ProviderBuild && (patch.BuildSuperEntitled != nil || patch.BuildRouteMode != nil) {
			return fmt.Errorf("Build administration fields require a Build account")
		}
		fields := make(map[string]any)
		if patch.Name != nil {
			fields["name"] = *patch.Name
		}
		if patch.Enabled != nil {
			result.EnabledChanged = current.Enabled != *patch.Enabled
			current.Enabled = *patch.Enabled
			fields["enabled"] = *patch.Enabled
		}
		if patch.Priority != nil {
			fields["priority"] = *patch.Priority
		}
		if patch.MaxConcurrent != nil {
			fields["max_concurrent"] = *patch.MaxConcurrent
		}
		if patch.MinimumRemaining != nil {
			fields["minimum_remaining"] = *patch.MinimumRemaining
		}
		if patch.BuildSuperEntitled != nil {
			fields["build_super_entitled"] = *patch.BuildSuperEntitled
		}
		if patch.BuildRouteMode != nil {
			fields["build_route_mode"] = string(*patch.BuildRouteMode)
		}
		if patch.Risk != nil {
			for name, value := range riskAttributionFields(*patch.Risk) {
				fields[name] = value
			}
		}
		if len(fields) > 0 {
			if err := tx.Model(&accountModel{}).Where("id = ?", id).Updates(fields).Error; err != nil {
				return err
			}
			changed = true
		}
		if patch.EncryptedCloudflareCookie != nil {
			write := tx.Model(&accountCredentialModel{}).Where("account_id = ?", id).Update("encrypted_cloudflare_cookie", *patch.EncryptedCloudflareCookie)
			if write.Error != nil {
				return write.Error
			}
			if write.RowsAffected != 1 {
				return repository.ErrNotFound
			}
			changed = true
		}
		if patch.Enabled != nil {
			_, err := deleteInvalidEgressLeaseBlocksForAccount(tx, current)
			return err
		}
		return nil
	})
	if err != nil {
		return repository.AccountAdminResult{}, mapError(err)
	}
	if changed {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: provider, AccountID: id})
	}
	result.Credential, err = r.Get(ctx, id)
	return result, err
}
