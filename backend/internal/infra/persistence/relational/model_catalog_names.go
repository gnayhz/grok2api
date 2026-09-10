package relational

import (
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// reconcileCatalogNames runs inside the catalog's namespace-locked transaction.
// Its result must be committed with the new primary names in that transaction.
func reconcileCatalogNames(tx *gorm.DB, matched map[int]modelRouteModel, values []model.Route) (map[uint64]model.NameSource, error) {
	sources := make(map[uint64]model.NameSource, len(matched))
	groups := make(map[string]map[uint64]bool, len(values))
	owners := make(map[uint64]modelRouteModel, len(matched))
	for index, value := range values {
		if groups[value.PublicID] == nil {
			groups[value.PublicID] = make(map[uint64]bool)
		}
		if row, ok := matched[index]; ok {
			groups[value.PublicID][row.ID] = true
			owners[row.ID] = row
			sources[row.ID] = model.NameSourceGenerated
			if row.PublicID == value.PublicID {
				sources[row.ID] = model.NameSource(row.NameSource)
			}
		}
	}
	for name, ids := range groups {
		var aliases []modelRouteAliasModel
		if err := tx.Select(modelAliasNameColumns).Where("alias = ?", name).Find(&aliases).Error; err != nil {
			return nil, err
		}
		for _, alias := range aliases {
			source := model.NameSource(alias.NameSource)
			_, retained := owners[alias.ModelRouteID]
			switch model.CatalogAliasPromotion(source, ids[alias.ModelRouteID], retained, alias.ReplacedByCatalog) {
			case model.AliasPromotionConflict:
				return nil, fmt.Errorf("%w: 模型公开 ID %q 已被路由 %d 保留为兼容名称", repository.ErrConflict, name, alias.ModelRouteID)
			case model.AliasPromotionKeepArchived:
				continue
			case model.AliasPromotionArchive:
				if err := tx.Model(&modelRouteAliasModel{}).Where("alias = ? AND model_route_id = ?", name, alias.ModelRouteID).Update("replaced_by_catalog", true).Error; err != nil {
					return nil, err
				}
				continue
			case model.AliasPromotionRestore:
				sources[alias.ModelRouteID] = model.MergeNameSource(sources[alias.ModelRouteID], source)
			}
			if err := tx.Delete(&modelRouteAliasModel{}, "alias = ? AND model_route_id = ?", name, alias.ModelRouteID).Error; err != nil {
				return nil, err
			}
		}
	}
	for index, row := range matched {
		if row.PublicID != values[index].PublicID && !model.RetireCatalogName(toModelDomain(row), row.PublicID, model.NameSource(row.NameSource)) {
			if err := preserveModelRouteAlias(tx, row.PublicID, row.ID, model.NameSource(row.NameSource)); err != nil {
				return nil, err
			}
		}
	}
	// Read only relationships belonging to retained catalog targets. Name equality
	// alone is never authority to remove another route's relationship.
	if len(owners) > 0 {
		ids := make([]uint64, 0, len(owners))
		for id := range owners {
			ids = append(ids, id)
		}
		var aliases []modelRouteAliasModel
		if err := tx.Select(modelAliasNameColumns).Where("model_route_id IN ?", ids).Find(&aliases).Error; err != nil {
			return nil, err
		}
		for _, alias := range aliases {
			if !model.RetireCatalogName(toModelDomain(owners[alias.ModelRouteID]), alias.Alias, model.NameSource(alias.NameSource)) {
				continue
			}
			if err := tx.Delete(&modelRouteAliasModel{}, "alias = ? AND model_route_id = ?", alias.Alias, alias.ModelRouteID).Error; err != nil {
				return nil, err
			}
		}
	}
	return sources, nil
}
