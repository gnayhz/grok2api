package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ResponseRepository struct{ db *Database }

func NewResponseRepository(db *Database) *ResponseRepository { return &ResponseRepository{db: db} }

func (r *ResponseRepository) Save(ctx context.Context, value inferencedomain.ResponseOwnership) error {
	row := responseOwnershipModel{
		ResponseID: value.ResponseID, AccountID: value.AccountID,
		ClientKeyID: value.ClientKeyID, ModelRouteID: value.ModelRouteID, Provider: string(value.Provider),
		PromptCacheKey: value.PromptCacheKey, ReasoningReplayKey: value.ReasoningReplayKey,
		ExpiresAt: value.ExpiresAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	// A response identity can be acknowledged again, but can never be rebound
	// to another client, account, route or continuity scope by an upstream ID
	// collision. The conflict predicate and insert execute atomically.
	result := r.db.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "response_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"expires_at": gorm.Expr("CASE WHEN response_ownership.expires_at > excluded.expires_at THEN response_ownership.expires_at ELSE excluded.expires_at END"),
			"updated_at": gorm.Expr("CASE WHEN response_ownership.updated_at > excluded.updated_at THEN response_ownership.updated_at ELSE excluded.updated_at END"),
		}),
		Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: `response_ownership.account_id = excluded.account_id
			AND response_ownership.client_key_id = excluded.client_key_id
			AND response_ownership.model_route_id = excluded.model_route_id
			AND response_ownership.provider = excluded.provider
			AND response_ownership.prompt_cache_key = excluded.prompt_cache_key
			AND response_ownership.reasoning_replay_key = excluded.reasoning_replay_key`}}},
	}).Create(&row)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (r *ResponseRepository) Get(ctx context.Context, responseID string, clientKeyID uint64, now time.Time) (inferencedomain.ResponseOwnership, error) {
	var row responseOwnershipModel
	if err := r.db.db.WithContext(ctx).Where("response_id = ? AND client_key_id = ? AND expires_at > ?", responseID, clientKeyID, now).First(&row).Error; err != nil {
		return inferencedomain.ResponseOwnership{}, mapError(err)
	}
	return inferencedomain.ResponseOwnership{
		ResponseID: row.ResponseID, AccountID: row.AccountID,
		ClientKeyID: row.ClientKeyID, ModelRouteID: row.ModelRouteID, Provider: account.Provider(row.Provider),
		PromptCacheKey: row.PromptCacheKey, ReasoningReplayKey: row.ReasoningReplayKey,
		ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *ResponseRepository) Delete(ctx context.Context, responseID string, clientKeyID uint64) error {
	result := r.db.db.WithContext(ctx).Where("response_id = ? AND client_key_id = ?", responseID, clientKeyID).Delete(&responseOwnershipModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *ResponseRepository) DeleteExpired(ctx context.Context, now time.Time, ownershipLimit, webStateLimit int) (repository.ResponseCleanupResult, error) {
	result := repository.ResponseCleanupResult{}
	if ownershipLimit > 0 {
		deleted, err := r.deleteExpiredOwnership(ctx, now, ownershipLimit)
		if err != nil {
			return result, err
		}
		result.OwnershipDeleted = deleted
		result.HasMore = result.HasMore || deleted >= int64(ownershipLimit)
	}
	if webStateLimit > 0 {
		deleted, err := r.deleteExpiredWebState(ctx, now, webStateLimit)
		if err != nil {
			return result, err
		}
		result.WebStateDeleted = deleted
		result.HasMore = result.HasMore || deleted >= int64(webStateLimit)
	}
	return result, nil
}

func (r *ResponseRepository) deleteExpiredOwnership(ctx context.Context, now time.Time, limit int) (int64, error) {
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := tx.Model(&responseOwnershipModel{}).
			Select("response_id").
			Where("expires_at <= ?", now).
			Order("expires_at ASC, response_id ASC").
			Limit(limit)
		result := tx.Where("response_id IN (?)", ids).Delete(&responseOwnershipModel{})
		deleted = result.RowsAffected
		return result.Error
	})
	return deleted, err
}

func (r *ResponseRepository) deleteExpiredWebState(ctx context.Context, now time.Time, limit int) (int64, error) {
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := tx.Model(&webResponseStateModel{}).
			Select("response_id").
			Where("expires_at <= ?", now).
			Order("expires_at ASC, response_id ASC").
			Limit(limit)
		result := tx.Where("response_id IN (?)", ids).Delete(&webResponseStateModel{})
		deleted = result.RowsAffected
		return result.Error
	})
	return deleted, err
}

func (r *ResponseRepository) SaveWebState(ctx context.Context, value inferencedomain.WebResponseState) error {
	row := webResponseStateModel{
		ResponseID: value.ResponseID, AccountID: value.AccountID, ConversationID: value.ConversationID,
		UpstreamParentResponseID: value.UpstreamParentResponseID, ResponseJSON: value.ResponseJSON,
		Status: value.Status, ExpiresAt: value.ExpiresAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	result := r.db.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "response_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"expires_at": gorm.Expr("CASE WHEN web_response_states.expires_at > excluded.expires_at THEN web_response_states.expires_at ELSE excluded.expires_at END"),
			"updated_at": gorm.Expr("CASE WHEN web_response_states.updated_at > excluded.updated_at THEN web_response_states.updated_at ELSE excluded.updated_at END"),
		}),
		Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: `web_response_states.account_id = excluded.account_id
			AND web_response_states.conversation_id = excluded.conversation_id
			AND web_response_states.upstream_parent_response_id = excluded.upstream_parent_response_id
			AND web_response_states.response_json = excluded.response_json
			AND web_response_states.status = excluded.status`}}},
	}).Create(&row)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (r *ResponseRepository) GetWebState(ctx context.Context, responseID string, now time.Time) (inferencedomain.WebResponseState, error) {
	var row webResponseStateModel
	if err := r.db.db.WithContext(ctx).Where("response_id = ? AND expires_at > ?", responseID, now).First(&row).Error; err != nil {
		return inferencedomain.WebResponseState{}, mapError(err)
	}
	return inferencedomain.WebResponseState{
		ResponseID: row.ResponseID, AccountID: row.AccountID, ConversationID: row.ConversationID,
		UpstreamParentResponseID: row.UpstreamParentResponseID, ResponseJSON: row.ResponseJSON,
		Status: row.Status, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *ResponseRepository) DeleteWebState(ctx context.Context, responseID string) error {
	result := r.db.db.WithContext(ctx).Where("response_id = ?", responseID).Delete(&webResponseStateModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}
