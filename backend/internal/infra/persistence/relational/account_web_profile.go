package relational

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ApplyWebProfile commits only the observed material's known success. Profile
// timestamps are independent of tier, network identity and quota snapshots.
func (r *AccountRepository) ApplyWebProfile(ctx context.Context, observed account.CredentialRef, event account.WebProfileObservation) (account.WebProfileResult, error) {
	var result account.WebProfileResult
	if observed.AccountID == 0 || observed.Provider != account.ProviderWeb {
		return result, repository.ErrNotFound
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockProviderAccount(tx, observed.AccountID, observed.Provider); err != nil {
			return err
		}
		var material accountCredentialModel
		if err := tx.Select("account_id", "generation").Where("account_id = ?", observed.AccountID).First(&material).Error; err != nil {
			return err
		}
		var profile webAccountProfileModel
		if err := tx.Select("account_id", "nsfw_enabled_at", "terms_accepted_at", "terms_accepted_version", "birth_date_set_at").Where("account_id = ?", observed.AccountID).Find(&profile).Error; err != nil {
			return err
		}
		var err error
		result, err = account.TransitionWebProfile(account.WebProfileState{Material: account.CredentialRef{AccountID: observed.AccountID, Provider: observed.Provider, Generation: material.Generation}, NSFWEnabledAt: profile.NSFWEnabledAt, TermsAcceptedAt: profile.TermsAcceptedAt, TermsAcceptedVersion: profile.TermsAcceptedVersion, BirthDateSetAt: profile.BirthDateSetAt}, observed, event)
		if err != nil || !result.Changed {
			return err
		}
		next := result.State
		profile = webAccountProfileModel{AccountID: observed.AccountID, Tier: string(account.WebTierAuto), NSFWEnabledAt: next.NSFWEnabledAt, TermsAcceptedAt: next.TermsAcceptedAt, TermsAcceptedVersion: next.TermsAcceptedVersion, BirthDateSetAt: next.BirthDateSetAt}
		columns := []string{"nsfw_enabled_at", "terms_accepted_at", "terms_accepted_version", "birth_date_set_at"}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.AssignmentColumns(columns)}).Create(&profile).Error
	})
	if err != nil {
		return account.WebProfileResult{}, mapError(err)
	}
	if result.Changed {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderWeb, AccountID: observed.AccountID})
	}
	return result, nil
}

// Both explicit import and successful identity correction call the same domain
// identity rule under the account lock. Incoming explicit import markers may
// subsequently describe the newly installed identity; other profile fields stay.
func resetWebProfileForChangedIdentity(tx *gorm.DB, providerValue account.Provider, id uint64, previousUserID, nextUserID string) error {
	if providerValue != account.ProviderWeb || !account.WebProfileIdentityChanged(previousUserID, nextUserID) {
		return nil
	}
	return tx.Model(&webAccountProfileModel{}).Where("account_id = ?", id).Updates(map[string]any{"nsfw_enabled_at": nil, "terms_accepted_at": nil, "terms_accepted_version": 0, "birth_date_set_at": nil}).Error
}
