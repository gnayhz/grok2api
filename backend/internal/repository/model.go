package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

// ModelRouteUnavailableError means the requested name resolved to configured
// routes, but none were enabled and supported by an active account. Enabled
// distinguishes a disabled name from an enabled name with no available target.
// Unsupported means every enabled target violates the M05 product contract.
// It retains ErrNotFound compatibility without allowing name/alias fallbacks.
type ModelRouteUnavailableError struct {
	Enabled     bool
	Unsupported bool
}

func (e *ModelRouteUnavailableError) Error() string {
	if e.Unsupported {
		return model.ErrUnsupportedCapability.Error()
	}
	if e.Enabled {
		return "configured model has no available account"
	}
	return "configured model is disabled"
}
func (e *ModelRouteUnavailableError) Unwrap() error { return ErrNotFound }

// ModelRepository 定义公开模型路由持久化能力。
type ModelRepository interface {
	// ReadPublicSnapshot freezes all names and projects account availability for
	// the requested tiers. Provider and routeID authorization remain in M05/M08.
	ReadPublicSnapshot(ctx context.Context, tiers []string) (model.PublicSnapshot, error)
	List(ctx context.Context, query ModelListQuery) ([]model.Route, int64, error)
	ListGroups(ctx context.Context, query ModelListQuery) ([]model.RouteGroup, int64, error)
	ListEnabled(ctx context.Context) ([]model.Route, error)
	ListEnabledForScope(ctx context.Context, filter ModelListFilter) ([]model.Route, error)
	ListConfiguredEnabled(ctx context.Context) ([]model.Route, error)
	Get(ctx context.Context, id uint64) (model.Route, error)
	GetByPublicID(ctx context.Context, publicID string) (model.Route, error)
	// GetByPublicIDCandidates fixes name identity before availability filtering.
	// A configured but unavailable name returns ModelRouteUnavailableError;
	// plain ErrNotFound means no configured name or persisted alias exists.
	GetByPublicIDCandidates(ctx context.Context, publicID string) ([]model.Route, error)
	// HasEnabledRouteByPublicID 不带账号可用性谓词，仅判断已启用路由是否存在
	//（用于区分 404 模型不存在与 503 无可用账号）。
	HasEnabledRouteByPublicID(ctx context.Context, publicID string) (bool, error)
	GetByProviderUpstream(ctx context.Context, provider account.Provider, upstreamModel string) (model.Route, error)
	// MergeRoutes inserts M05 publication decisions without overwriting existing
	// managed routes or aliases. It does not derive names or capabilities.
	MergeRoutes(ctx context.Context, provider account.Provider, values []model.Route) error
	ReplaceProviderRoutes(ctx context.Context, provider account.Provider, values []model.Route) error
	BeginAccountCapabilitySync(ctx context.Context, accountID uint64, attemptedAt time.Time) (model.CapabilitySyncRef, error)
	CompleteAccountCapabilitySync(ctx context.Context, ref model.CapabilitySyncRef, result model.CapabilitySyncResult) error
	HasSuccessfulAccountSync(ctx context.Context, accountID uint64) (bool, error)
	ListStaleAccountSyncIDs(ctx context.Context, before time.Time, limit int) ([]uint64, error)
	Create(ctx context.Context, value model.Route, accountIDs []uint64) (model.Route, error)
	Patch(ctx context.Context, id uint64, patch model.RoutePatch) (model.Route, error)
	Delete(ctx context.Context, id uint64) error
	DeleteMany(ctx context.Context, ids []uint64) (int64, error)
	UpdateManyEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error)
}
