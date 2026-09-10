package relational

import (
	"context"
	"database/sql"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"gorm.io/gorm"
)

// ReadPublicSnapshot freezes names, aliases and account-scope facts together.
// No per-route annotation or per-alias lookup is needed by public discovery.
func (r *ModelRepository) ReadPublicSnapshot(ctx context.Context, tiers []string) (model.PublicSnapshot, error) {
	var result model.PublicSnapshot
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tier := modelTierAvailabilityPredicateWithAvailability(tiers, true)
		if tier == "" {
			tier = "TRUE"
		}
		var rows []struct {
			Route            modelRouteModel `gorm:"embedded"`
			AccountAvailable bool
			ScopeAvailable   bool
		}
		// Name provenance is needed by writers, not this public projection. Keep
		// it out of the hot snapshot's per-route SQL decoding and allocations.
		const columns = "model_routes.id, model_routes.public_id, model_routes.provider, model_routes.upstream_model, model_routes.capability, model_routes.origin, model_routes.enabled, model_routes.created_at, model_routes.updated_at"
		if err := tx.Model(&modelRouteModel{}).Select(columns+", ("+availableRoutePredicate+") AS account_available, ("+tier+") AS scope_available", true, account.AuthStatusActive).Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
			return err
		}
		result.Routes = make([]model.PublicRoute, 0, len(rows))
		for _, row := range rows {
			result.Routes = append(result.Routes, model.PublicRoute{Route: toModelDomain(row.Route), AccountAvailable: row.AccountAvailable, ScopeAvailable: row.ScopeAvailable})
		}
		var aliases []modelRouteAliasModel
		if err := tx.Select("alias, model_route_id").Where("replaced_by_catalog = ?", false).Order("alias, model_route_id").Find(&aliases).Error; err != nil {
			return err
		}
		result.Aliases = make([]model.PersistedAlias, 0, len(aliases))
		for _, alias := range aliases {
			result.Aliases = append(result.Aliases, model.PersistedAlias{Name: alias.Alias, RouteID: alias.ModelRouteID})
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return model.PublicSnapshot{}, err
	}
	return result, nil
}
