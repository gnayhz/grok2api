package relational

// 账号清理存储实现(MaintenanceRunner 存储侧):候选拉取、
// 批量删除与关联计数。行类型与建库在 account_repository.go。

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

func (r *AccountRepository) DeleteAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, candidateIDs []uint64) ([]uint64, error) {
	if len(candidateIDs) == 0 {
		return []uint64{}, nil
	}
	deletedIDs := make([]uint64, 0, len(candidateIDs))
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}
		deletable, err := excludeAccountsWithActiveMediaJobs(tx, candidateIDs)
		if err != nil {
			return err
		}
		if len(deletable) == 0 {
			return nil
		}

		var lockedIDs []uint64
		lockQuery := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ? AND auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", deletable, account.AuthStatusReauthRequired, markedBefore.UTC()).
			Where(accountUnclassifiedRefreshReauthPredicate)
		if !includeDisabled {
			lockQuery = lockQuery.Where("enabled = ?", true)
		}
		if err := lockQuery.Pluck("id", &lockedIDs).Error; err != nil {
			return err
		}
		// lock 后再过滤活动视频任务，避免 list 与 delete 之间的 TOCTOU。
		lockedIDs, err = excludeAccountsWithActiveMediaJobs(tx, lockedIDs)
		if err != nil {
			return err
		}
		if len(lockedIDs) == 0 {
			return nil
		}
		deletion := tx.Where("id IN ? AND auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", lockedIDs, account.AuthStatusReauthRequired, markedBefore.UTC()).
			Where(accountUnclassifiedRefreshReauthPredicate)
		if !includeDisabled {
			deletion = deletion.Where("enabled = ?", true)
		}
		result := deletion.Delete(&accountModel{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == int64(len(lockedIDs)) {
			deletedIDs = append(deletedIDs, lockedIDs...)
			return nil
		}
		var remaining []uint64
		if err := tx.Model(&accountModel{}).Where("id IN ?", lockedIDs).Pluck("id", &remaining).Error; err != nil {
			return err
		}
		remainingSet := make(map[uint64]struct{}, len(remaining))
		for _, id := range remaining {
			remainingSet[id] = struct{}{}
		}
		for _, id := range lockedIDs {
			if _, exists := remainingSet[id]; !exists {
				deletedIDs = append(deletedIDs, id)
			}
		}
		return nil
	})
	if err == nil && len(deletedIDs) > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return deletedIDs, err
}

func (r *AccountRepository) ListAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, afterID uint64, limit int) ([]uint64, error) {
	if limit < 1 {
		limit = 100
	}
	query := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Select("id").
		Where("auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", account.AuthStatusReauthRequired, markedBefore.UTC()).
		Where(accountUnclassifiedRefreshReauthPredicate).
		Where("NOT EXISTS (SELECT 1 FROM media_jobs job WHERE job.account_id = provider_accounts.id AND job.status IN ?)", []string{string(media.StatusQueued), string(media.StatusInProgress)})
	if afterID > 0 {
		query = query.Where("id > ?", afterID)
	}
	if !includeDisabled {
		query = query.Where("enabled = ?", true)
	}
	var candidates []uint64
	err := query.Order("id ASC").Limit(limit).Pluck("id", &candidates).Error
	return candidates, err
}
