package relational

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *AccountRepository) GetQuotaRevision(ctx context.Context, accountID uint64) (uint64, error) {
	var row quotaStateModel
	err := r.db.db.WithContext(ctx).Where("account_id = ?", accountID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var count int64
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", accountID).Count(&count).Error; err != nil {
			return 0, err
		}
		if count == 0 {
			return 0, repository.ErrNotFound
		}
		return 0, nil
	}
	return row.Revision, err
}

// The no-op UPDATE obtains a row write lock on PostgreSQL and a write
// transaction on SQLite before reading receipts. No process-local mutex owns
// this state. SELECT FROM the account table cannot create a deleted account.
func lockQuotaState(tx *gorm.DB, accountID uint64) (bool, error) {
	if err := tx.Exec("INSERT INTO account_quota_state (account_id, revision) SELECT id, 0 FROM provider_accounts WHERE id = ? ON CONFLICT (account_id) DO NOTHING", accountID).Error; err != nil {
		return false, err
	}
	result := tx.Model(&quotaStateModel{}).Where("account_id = ?", accountID).UpdateColumn("revision", gorm.Expr("revision"))
	return result.RowsAffected == 1, result.Error
}

func bumpQuotaRevision(tx *gorm.DB, accountID uint64) error {
	result := tx.Model(&quotaStateModel{}).Where("account_id = ? AND revision < ?", accountID, int64(math.MaxInt64)).UpdateColumn("revision", gorm.Expr("revision + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return repository.ErrConflict
	}
	return nil
}

func (r *AccountRepository) SaveQuotaSnapshot(ctx context.Context, value repository.QuotaSnapshotWrite) error {
	if value.AccountID == 0 || value.Revision >= math.MaxInt64 || value.ReplaceAll && len(value.ReplaceModes) > 0 {
		return repository.ErrConflict
	}
	allowed := make(map[string]bool, len(value.ReplaceModes))
	for _, mode := range value.ReplaceModes {
		if mode == "" || len(mode) > 64 || strings.TrimSpace(mode) != mode || allowed[mode] {
			return repository.ErrConflict
		}
		allowed[mode] = true
	}
	seen := make(map[string]bool, len(value.Windows))
	for _, window := range value.Windows {
		if window.Mode == "" || len(window.Mode) > 64 || strings.TrimSpace(window.Mode) != window.Mode || seen[window.Mode] || len(allowed) > 0 && !allowed[window.Mode] {
			return repository.ErrConflict
		}
		seen[window.Mode] = true
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		exists, err := lockQuotaState(tx, value.AccountID)
		if err != nil {
			return err
		}
		if !exists {
			return repository.ErrNotFound
		}
		result := tx.Model(&quotaStateModel{}).Where("account_id = ? AND revision = ?", value.AccountID, value.Revision).UpdateColumn("revision", value.Revision+1)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return repository.ErrConflict
		}
		if err := writeQuotaWindows(tx, value.AccountID, value.Tier, value.SyncedAt, value.Windows, value.ReplaceAll, value.ReplaceModes, value.Revision+1); err != nil {
			return err
		}
		pending := tx.Model(&quotaConsumptionModel{}).Where("account_id = ? AND state = ?", value.AccountID, account.QuotaConsumptionPendingRefresh)
		if !value.ReplaceAll {
			modes := value.ReplaceModes
			if len(modes) == 0 {
				for _, window := range value.Windows {
					modes = append(modes, window.Mode)
				}
			}
			if len(modes) == 0 {
				return nil
			}
			pending = pending.Where("mode IN ?", modes)
		}
		return pending.Updates(map[string]any{"state": account.QuotaConsumptionRefreshed, "updated_at": value.SyncedAt}).Error
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: value.AccountID})
	}
	return err
}

func quotaConsumptionReceipt(row quotaConsumptionModel) account.QuotaConsumptionReceipt {
	return account.QuotaConsumptionReceipt{QuotaConsumption: account.QuotaConsumption{EventID: row.EventID, AccountID: row.AccountID, Mode: row.Mode, SnapshotVersion: row.SnapshotVersion, Units: row.Units}, State: account.QuotaConsumptionState(row.State)}
}

func (r *AccountRepository) ConsumeQuota(ctx context.Context, value account.QuotaConsumption, now time.Time) (account.QuotaConsumptionReceipt, error) {
	if err := value.Validate(); err != nil {
		return account.QuotaConsumptionReceipt{}, err
	}
	var receipt account.QuotaConsumptionReceipt
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) (err error) {
		defer func() {
			if err != nil {
				return
			}
			var window quotaWindowModel
			loadErr := tx.Where("account_id = ? AND mode = ?", value.AccountID, value.Mode).Take(&window).Error
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				return
			}
			if loadErr != nil {
				err = loadErr
				return
			}
			receipt.Projection = &account.QuotaProjection{Mode: window.Mode, SnapshotVersion: window.SnapshotVersion, Revision: window.Revision, Remaining: window.Remaining}
		}()
		exists, err := lockQuotaState(tx, value.AccountID)
		if err != nil {
			return err
		}
		row := quotaConsumptionModel{EventID: value.EventID, AccountID: value.AccountID, Mode: value.Mode, SnapshotVersion: value.SnapshotVersion, Units: value.Units,
			State: string(account.QuotaConsumptionPendingRefresh), CreatedAt: now, UpdatedAt: now}
		if !exists {
			row.State = string(account.QuotaConsumptionAccountDeleted)
		}
		inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if inserted.Error != nil {
			return inserted.Error
		}
		if inserted.RowsAffected == 0 {
			if err := tx.Where("event_id = ?", value.EventID).Take(&row).Error; err != nil {
				return err
			}
			receipt = quotaConsumptionReceipt(row)
			if receipt.QuotaConsumption != value {
				return repository.ErrConflict
			}
			if !exists && receipt.State == account.QuotaConsumptionPendingRefresh {
				receipt.State = account.QuotaConsumptionAccountDeleted
				return tx.Model(&quotaConsumptionModel{}).Where("event_id = ?", value.EventID).Updates(map[string]any{"state": receipt.State, "updated_at": now}).Error
			}
			return nil
		}
		if exists {
			if err := bumpQuotaRevision(tx, value.AccountID); err != nil {
				return err
			}
			var window quotaWindowModel
			windowErr := tx.Where("account_id = ? AND mode = ?", value.AccountID, value.Mode).Take(&window).Error
			if windowErr != nil && !errors.Is(windowErr, gorm.ErrRecordNotFound) {
				return windowErr
			}
			if windowErr == nil && value.CanApply(toQuotaWindowDomain(window), now) {
				updated := tx.Model(&quotaWindowModel{}).Where("account_id = ? AND mode = ? AND snapshot_version = ?", value.AccountID, value.Mode, value.SnapshotVersion).
					Updates(map[string]any{"remaining": gorm.Expr("CASE WHEN remaining <= ? THEN 0 ELSE remaining - ? END", value.Units, value.Units), "updated_at": now, "revision": gorm.Expr("(SELECT revision FROM account_quota_state WHERE account_id = ?)", value.AccountID)})
				if updated.Error != nil {
					return updated.Error
				}
				if updated.RowsAffected == 1 {
					row.State = string(account.QuotaConsumptionApplied)
					if err := tx.Model(&quotaConsumptionModel{}).Where("event_id = ?", row.EventID).UpdateColumn("state", row.State).Error; err != nil {
						return err
					}
				}
			}
		}
		receipt = quotaConsumptionReceipt(row)
		return nil
	})
	if err == nil && receipt.Projection != nil {
		// Duplicate acknowledgements also repair a lost invalidation. Consumers
		// must not apply a second in-memory delta on top of this projection.
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: value.AccountID, Quota: receipt.Projection})
	}
	return receipt, err
}

func (r *AccountRepository) ListPendingQuotaRefreshes(ctx context.Context, afterID uint64, limit int) ([]account.PendingQuotaRefresh, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ids []uint64
	db := r.db.db.WithContext(ctx)
	if err := db.Model(&quotaConsumptionModel{}).Where("state = ? AND account_id > ?", account.QuotaConsumptionPendingRefresh, afterID).Distinct("account_id").Order("account_id ASC").Limit(limit).Pluck("account_id", &ids).Error; err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var values []account.PendingQuotaRefresh
	err := db.Model(&quotaConsumptionModel{}).Select("DISTINCT account_id, mode").Where("state = ? AND account_id IN ?", account.QuotaConsumptionPendingRefresh, ids).Order("account_id ASC, mode ASC").Scan(&values).Error
	return values, err
}

// ResolveDeletedQuotaConsumptions closes pending work only after an atomic
// absence check. Existing applied receipts remain valid after account deletion.
func (r *AccountRepository) ResolveDeletedQuotaConsumptions(ctx context.Context, accountID uint64, now time.Time) error {
	return r.db.db.WithContext(ctx).Model(&quotaConsumptionModel{}).
		Where("account_id = ? AND state = ? AND NOT EXISTS (SELECT 1 FROM provider_accounts WHERE id = ?)", accountID, account.QuotaConsumptionPendingRefresh, accountID).
		Updates(map[string]any{"state": account.QuotaConsumptionAccountDeleted, "updated_at": now}).Error
}
