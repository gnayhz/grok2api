package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// ApplyCredential serializes with imports and administrator/health writes by
// taking the account lock first. No network work or callback runs in this tx.
func (r *AccountRepository) ApplyCredential(ctx context.Context, ref account.CredentialRef, event account.CredentialEvent) (account.CredentialResult, error) {
	var result account.CredentialResult
	if ref.AccountID == 0 || !ref.Provider.IsValid() {
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
		now := event.OccurredAt
		if now.IsZero() {
			now = time.Now().UTC()
		}
		var err error
		result, err = account.TransitionCredential(toAccountDomain(row), ref, event, now)
		if err != nil || !result.Applied {
			return err
		}
		next := result.Credential
		// Authentication and its diagnostic are independent of health/risk/admin.
		if err := tx.Model(&accountModel{}).Where("id = ?", ref.AccountID).Updates(map[string]any{
			"auth_status": string(next.AuthStatus), "auth_error": next.AuthError, "reauth_marked_at": next.ReauthMarkedAt,
		}).Error; err != nil {
			return err
		}
		row.AuthStatus = string(next.AuthStatus)
		if _, err := deleteInvalidEgressLeaseBlocksForAccount(tx, row); err != nil {
			return err
		}
		if event.Kind == account.CredentialRejected {
			return nil
		}
		material := fromAccountCredentialDomain(next)
		fields := map[string]any{
			"refresh_due_at": material.RefreshDueAt, "last_refresh_at": material.LastRefreshAt,
			"refresh_failures": material.RefreshFailures, "refresh_unclassified_auth_failures": material.RefreshUnclassifiedAuthFailures,
			"last_refresh_error_status": material.LastRefreshErrorStatus, "last_refresh_error": material.LastRefreshError,
			"last_refresh_error_message": material.LastRefreshErrorMessage, "last_refresh_error_response": material.LastRefreshErrorResponse,
			"refresh_permanent": material.RefreshPermanent, "updated_at": time.Now().UTC(),
		}
		if event.Kind == account.CredentialRefreshed {
			fields["generation"], fields["encrypted_primary"], fields["encrypted_refresh"] = material.Generation, material.EncryptedPrimary, material.EncryptedRefresh
			fields["expires_at"], fields["build_bot_flag_source"] = material.ExpiresAt, material.BuildBotFlagSource
		}
		return tx.Model(&accountCredentialModel{}).Where("account_id = ?", ref.AccountID).Updates(fields).Error
	})
	if err != nil {
		return account.CredentialResult{}, mapError(err)
	}
	if result.Applied {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, Provider: ref.Provider, AccountID: ref.AccountID})
	}
	// Return the complete current projection, including profiles used downstream.
	// A read failure after commit is an uncertain acknowledgement, not a rollback.
	current, err := r.Get(ctx, ref.AccountID)
	if err != nil {
		return account.CredentialResult{}, err
	}
	result.Credential = current
	return result, nil
}
