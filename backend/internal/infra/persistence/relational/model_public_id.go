package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type modelPublicIDMigration struct {
	ID       uint64
	Previous string
	Current  string
	Source   modeldomain.NameSource
}

// Explicit columns make name governance safe on connections opened before an
// additive SQLite schema migration: SELECT * can expose cached old columns on
// the first statement after another connection's DDL.
const modelRouteNameColumns = "id, public_id, provider, upstream_model, capability, origin, name_source, enabled, created_at, updated_at"
const modelAliasNameColumns = "alias, model_route_id, name_source, replaced_by_catalog, created_at"

// ensureCanonicalModelPublicIDs 原位迁移内部路由 ID，保留路由主键和所有下游授权关系。
func (d *Database) ensureCanonicalModelPublicIDs(ctx context.Context) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockModelNamespaces(tx, account.Providers()...); err != nil {
			return err
		}
		var rows []modelRouteModel
		if err := tx.Select(modelRouteNameColumns).Order("id ASC").Find(&rows).Error; err != nil {
			return err
		}
		migrations := make([]modelPublicIDMigration, 0, len(rows))
		for _, row := range rows {
			providerValue := account.Provider(row.Provider)
			publicID, ok := modeldomain.NormalizePublicID(providerValue, row.PublicID)
			if !ok {
				return fmt.Errorf("模型路由 %d 的公开 ID %q 无法规范化到 %s", row.ID, row.PublicID, providerValue)
			}
			if publicID == row.PublicID {
				continue
			}
			source, err := modelPrimaryNameSource(tx, row.ID, publicID, modeldomain.NameSource(row.NameSource))
			if err != nil {
				return err
			}
			migrations = append(migrations, modelPublicIDMigration{ID: row.ID, Previous: row.PublicID, Current: publicID, Source: source})
		}
		for _, migration := range migrations {
			if err := ensureModelPublicIDNotAlias(tx, migration.Current, migration.ID); err != nil {
				return err
			}
			if err := preserveModelRouteAlias(tx, migration.Previous, migration.ID, migration.Source); err != nil {
				return err
			}
		}
		// 全部先移到不冲突的临时命名空间，允许 A/B 两条路由交换或释放目标名称。
		for _, migration := range migrations {
			temporary := fmt.Sprintf("__grok2api_model_namespace_%d", migration.ID)
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", migration.ID).Update("public_id", temporary).Error; err != nil {
				return mapError(err)
			}
		}
		for _, migration := range migrations {
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", migration.ID).Updates(map[string]any{"public_id": migration.Current, "name_source": migration.Source}).Error; err != nil {
				return fmt.Errorf("迁移模型路由 %d 到 %q: %w", migration.ID, migration.Current, mapError(err))
			}
		}
		return nil
	})
}

// Only a route that currently owns this name may preserve its relationship.
// Other current/previous members of the same name remain separate identities.
func preserveModelRouteAlias(tx *gorm.DB, alias string, routeID uint64, source modeldomain.NameSource) error {
	alias = strings.TrimSpace(alias)
	if alias == "" || routeID == 0 {
		return nil
	}
	var owner modelRouteModel
	if err := tx.Select("id").Where("id = ? AND public_id = ?", routeID, alias).First(&owner).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: route %d does not own public name %q", repository.ErrConflict, routeID, alias)
		}
		return err
	}
	merged, err := modelPrimaryNameSource(tx, routeID, alias, source)
	if err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "alias"}, {Name: "model_route_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"name_source", "replaced_by_catalog"}),
	}).Create(&modelRouteAliasModel{Alias: alias, ModelRouteID: routeID, NameSource: string(merged)}).Error
}

// All callers hold the Provider namespace lock. Reading and merging in this
// transaction shares M05's policy without encoding a second precedence in SQL.
func modelPrimaryNameSource(tx *gorm.DB, routeID uint64, name string, source modeldomain.NameSource) (modeldomain.NameSource, error) {
	var alias modelRouteAliasModel
	err := tx.Select("name_source").Where("alias = ? AND model_route_id = ?", name, routeID).First(&alias).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return modeldomain.MergeNameSource(source, source), nil
	}
	if err != nil {
		return "", err
	}
	return modeldomain.MergeNameSource(source, modeldomain.NameSource(alias.NameSource)), nil
}

func ensureModelPublicIDNotAlias(tx *gorm.DB, publicID string, routeID uint64) error {
	var aliases []modelRouteAliasModel
	if err := tx.Where("alias = ? AND replaced_by_catalog = ?", publicID, false).Find(&aliases).Error; err != nil {
		return err
	}
	if len(aliases) == 0 {
		return nil
	}
	for _, alias := range aliases {
		if routeID != 0 && alias.ModelRouteID == routeID {
			return nil
		}
	}
	// A currently named pool may accept additional targets as before; a name
	// held only by historical aliases cannot be claimed by an unrelated route.
	var direct modelRouteModel
	err := tx.Select("id").Where("public_id = ?", publicID).First(&direct).Error
	if err == nil {
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return fmt.Errorf("%w: 模型公开 ID %q 已被保留为兼容名称", repository.ErrConflict, publicID)
}
