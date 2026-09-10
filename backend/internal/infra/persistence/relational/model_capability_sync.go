package relational

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *ModelRepository) BeginAccountCapabilitySync(ctx context.Context, accountID uint64, attemptedAt time.Time) (model.CapabilitySyncRef, error) {
	var ref model.CapabilitySyncRef
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockProviderAccount(tx, accountID, ""); err != nil {
			return err
		}
		var state accountModelSyncStateModel
		err := tx.First(&state, "account_id = ?", accountID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if state.SyncRevision >= math.MaxInt64 {
			return model.ErrCapabilitySyncExhausted
		}
		state.AccountID = accountID
		state.SyncRevision++
		state.SyncPending = true
		state.LastAttemptAt = attemptedAt.UTC()
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.AssignmentColumns([]string{"sync_revision", "sync_pending", "last_attempt_at"})}).Create(&state).Error; err != nil {
			return err
		}
		ref = model.CapabilitySyncRef{AccountID: accountID, Revision: state.SyncRevision}
		return nil
	})
	if err != nil {
		return model.CapabilitySyncRef{}, mapError(err)
	}
	return ref, nil
}

// CompleteAccountCapabilitySync serializes with credential imports/refreshes
// using the same account-first lock. The version and material check, capability
// replacement and diagnostic write commit together; no upstream work holds SQL.
func (r *ModelRepository) CompleteAccountCapabilitySync(ctx context.Context, ref model.CapabilitySyncRef, result model.CapabilitySyncResult) error {
	if ref.AccountID == 0 || ref.Revision == 0 || result.Credential.AccountID != ref.AccountID || !result.Credential.Provider.IsValid() {
		return model.ErrCapabilitySyncSuperseded
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockProviderAccount(tx, ref.AccountID, result.Credential.Provider); err != nil {
			return err
		}
		var credential accountCredentialModel
		if err := tx.First(&credential, "account_id = ?", ref.AccountID).Error; err != nil {
			return err
		}
		if credential.Generation != result.Credential.Generation {
			return model.ErrCapabilitySyncSuperseded
		}
		var state accountModelSyncStateModel
		if err := tx.First(&state, "account_id = ?", ref.AccountID).Error; err != nil {
			return err
		}
		if state.SyncRevision != ref.Revision || !state.SyncPending {
			return model.ErrCapabilitySyncSuperseded
		}
		message := ""
		if result.Err != nil {
			message = truncate(result.Err.Error(), 512)
		}
		fields := map[string]any{"sync_pending": false, "last_error": message}
		if result.Err == nil {
			if err := tx.Where("account_id = ?", ref.AccountID).Delete(&accountModelCapabilityModel{}).Error; err != nil {
				return err
			}
			unique := make(map[string]struct{}, len(result.Models))
			rows := make([]accountModelCapabilityModel, 0, len(result.Models))
			for _, value := range result.Models {
				value = strings.TrimSpace(value)
				if value == "" {
					continue
				}
				if _, exists := unique[value]; exists {
					continue
				}
				unique[value] = struct{}{}
				rows = append(rows, accountModelCapabilityModel{AccountID: ref.AccountID, UpstreamModel: value})
			}
			if len(rows) > 0 {
				if err := tx.CreateInBatches(rows, 200).Error; err != nil {
					return err
				}
			}
			fields["last_success_at"] = state.LastAttemptAt
		}
		return tx.Model(&accountModelSyncStateModel{}).Where("account_id = ?", ref.AccountID).Updates(fields).Error
	})
	if err == nil && result.Err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, AccountID: ref.AccountID})
	}
	return mapError(err)
}
