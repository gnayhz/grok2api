package relational

import (
	"context"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func (r *AccountRepository) ApplyHealth(ctx context.Context, id uint64, provider account.Provider, event account.HealthEvent) (account.HealthResult, error) {
	var result account.HealthResult
	if event.Kind == account.HealthMigrateLegacyQuality {
		return result, errors.New("legacy health transfer requires atomic restriction migration")
	}
	if id == 0 || provider != "" && !provider.IsValid() {
		return result, repository.ErrNotFound
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := readLockedHealth(tx, id, provider)
		if err != nil {
			return err
		}
		result, err = applyHealthTransition(tx, current, event, time.Now().UTC())
		return err
	})
	if err != nil {
		return account.HealthResult{}, mapError(err)
	}
	if result.Applied {
		r.notifyHealth(ctx, result.State)
	}
	return result, nil
}

func readLockedHealth(tx *gorm.DB, id uint64, provider account.Provider) (account.HealthState, error) {
	if err := lockProviderAccount(tx, id, provider); err != nil {
		return account.HealthState{}, err
	}
	var row accountModel
	if err := tx.Select("id", "provider", "health_revision", "failure_count", "cooldown_until", "cooldown_marked_at", "last_error").First(&row, id).Error; err != nil {
		return account.HealthState{}, err
	}
	return toAccountDomain(row).HealthState(), nil
}

// Shared by ordinary events and the atomic legacy quality transfer.
func applyHealthTransition(tx *gorm.DB, current account.HealthState, event account.HealthEvent, now time.Time) (account.HealthResult, error) {
	result, err := account.TransitionHealth(current, event, now)
	if err != nil {
		return result, err
	}
	updates := map[string]any{}
	if result.Applied {
		next := result.State
		updates["health_revision"], updates["failure_count"], updates["cooldown_until"] = next.Revision, next.FailureCount, next.CooldownUntil
		updates["cooldown_marked_at"], updates["last_error"] = next.CooldownMarkedAt, next.LastError
	}
	if event.Kind == account.HealthSuccess {
		updates["last_used_at"] = now
	}
	if len(updates) > 0 {
		err = tx.Model(&accountModel{}).Where("id = ?", current.AccountID).Updates(updates).Error
	}
	return result, err
}

func (r *AccountRepository) notifyHealth(ctx context.Context, state account.HealthState) {
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountHealthChanged, Provider: state.Provider, AccountID: state.AccountID,
		HealthRevision: state.Revision, FailureCount: state.FailureCount, CooldownUntil: state.CooldownUntil, HealthMarker: account.NormalizeHealthMarker(state.LastError)})
}
