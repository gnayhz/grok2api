package relational

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"gorm.io/gorm"
)

func TestModelNameSourceLegacyMigrationAndPublication(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			// Recreate the exact pre-metadata column set before inserting facts.
			downgrade := func() error {
				return a.db.Transaction(func(tx *gorm.DB) error {
					for _, value := range []struct {
						row   any
						table string
					}{{&modelRouteModel{}, "model_routes"}, {&modelRouteAliasModel{}, "model_route_aliases"}} {
						if value.table == "model_route_aliases" {
							if err := tx.Migrator().DropConstraint(value.row, "chk_model_route_aliases_replacement"); err != nil {
								return err
							}
							if err := tx.Migrator().DropColumn(value.row, "replaced_by_catalog"); err != nil {
								return err
							}
						}
						if err := tx.Migrator().DropConstraint(value.row, "chk_"+value.table+"_name_source"); err != nil {
							return err
						}
						if err := tx.Migrator().DropColumn(value.row, "name_source"); err != nil {
							return err
						}
					}
					return nil
				})
			}
			var err error
			if dialect == "sqlite" {
				err = a.withSQLiteForeignKeysDisabled(ctx, downgrade)
			} else {
				err = downgrade()
			}
			if err != nil {
				t.Fatal(err)
			}
			when := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
			const fastID, qualityID = uint64(101), uint64(102)
			const retired = "grok-imagine-image-quality-lite"
			for _, row := range []struct {
				id             uint64
				name, upstream string
			}{{fastID, "Web/grok-imagine-image-lite", "grok-imagine-image"}, {qualityID, retired, "grok-imagine-image-quality"}} {
				if err := a.db.Exec("INSERT INTO model_routes (id, public_id, provider, upstream_model, capability, origin, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", row.id, row.name, account.ProviderWeb, row.upstream, model.CapabilityImage, model.OriginCatalog, true, when, when).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"Web/grok-imagine-image", "Web/grok-imagine-image-2.0"} {
				if err := a.db.Exec("INSERT INTO model_route_aliases (alias, model_route_id, created_at) VALUES (?, ?, ?)", name, fastID, when).Error; err != nil {
					t.Fatal(err)
				}
			}
			credential, _, err := NewAccountRepository(a).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, SourceKey: "legacy", Name: "legacy", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.db.Create(&modelRouteAccountModel{ModelRouteID: qualityID, AccountID: credential.ID}).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Create(&modelRouteAccountModel{ModelRouteID: fastID, AccountID: credential.ID}).Error; err != nil {
				t.Fatal(err)
			}
			key := clientKeyModel{ModelScope: "restricted", Name: "legacy-owner", Prefix: "legacy", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "fixture", Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
			if err := a.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Create(&clientKeyModelPermission{ClientKeyID: key.ID, ModelRouteID: qualityID}).Error; err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"model_routes", "model_route_aliases"} {
				if err := a.db.Exec("CREATE INDEX " + table + "_legacy_created ON " + table + "(created_at)").Error; err != nil {
					t.Fatal(err)
				}
				if dialect == "sqlite" {
					if err := a.db.Exec("CREATE TRIGGER " + table + "_legacy_trigger AFTER INSERT ON " + table + " BEGIN SELECT 1; END").Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			failure := errors.New("name source whole-schema rollback")
			hook := "g13_name_source_rollback"
			if err := a.db.Callback().Raw().Before("gorm:raw").Register(hook, func(tx *gorm.DB) {
				if strings.Contains(tx.Statement.SQL.String(), "CREATE INDEX IF NOT EXISTS idx_model_route_accounts_account_route") {
					_ = tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			err = a.InitializeSchema(ctx)
			_ = a.db.Callback().Raw().Remove(hook)
			if !errors.Is(err, failure) {
				t.Fatalf("injected failure not observed: %v", err)
			}
			for _, table := range []string{"model_routes", "model_route_aliases"} {
				if a.db.Migrator().HasColumn(table, "name_source") {
					t.Fatalf("failed migration committed %s source column", table)
				}
			}
			// Prime B with the legacy column layout; its first post-upgrade writer
			// must read real name sources despite SQLite's stale SELECT * metadata.
			var primed []map[string]any
			for _, table := range []string{"model_routes", "model_route_aliases"} {
				if err := b.db.Table(table).Find(&primed).Error; err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := a.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var before []modelRouteModel
			if err := a.db.Select(modelRouteNameColumns).Order("id").Find(&before).Error; err != nil || len(before) != 2 {
				t.Fatalf("migrated routes %+v %v", before, err)
			}
			for _, row := range before {
				if row.NameSource != string(model.NameSourceLegacy) || !row.CreatedAt.Equal(when) {
					t.Fatalf("invented source or changed creation: %+v", row)
				}
			}
			repo := NewModelRepository(b)
			for range 2 {
				if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, model.CatalogRoutes(account.ProviderWeb)); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{retired, "Web/" + retired, "grok-imagine-image"} {
				route, err := repo.GetByPublicID(ctx, name)
				if err != nil || route.ID != qualityID {
					t.Fatalf("legacy name %s changed target: %+v %v", name, route, err)
				}
			}
			candidates, err := repo.GetByPublicIDCandidates(ctx, "grok-imagine-image")
			if err != nil || len(candidates) != 1 || candidates[0].ID != qualityID {
				t.Fatalf("catalog product split retained wrong generation candidates: %+v %v", candidates, err)
			}
			for _, name := range []string{"Web/grok-imagine-image", "Web/grok-imagine-image-2.0"} {
				var alias modelRouteAliasModel
				if err := b.db.Select(modelAliasNameColumns).Where("alias = ? AND model_route_id = ?", name, fastID).First(&alias).Error; err != nil || alias.NameSource != string(model.NameSourceLegacy) || !alias.ReplacedByCatalog || !alias.CreatedAt.Equal(when) {
					t.Fatalf("legacy remap erased original edge: %+v %v", alias, err)
				}
			}
			for _, name := range []string{retired, "Web/" + retired} {
				var alias modelRouteAliasModel
				if err := b.db.Select(modelAliasNameColumns).Where("alias = ? AND model_route_id = ?", name, qualityID).First(&alias).Error; err != nil || alias.NameSource != string(model.NameSourceLegacy) || alias.ReplacedByCatalog {
					t.Fatalf("namespace/catalog rename lost source: %+v %v", alias, err)
				}
			}
			var count int64
			if err := b.db.Model(&clientKeyModelPermission{}).Where("client_key_id = ? AND model_route_id = ?", key.ID, qualityID).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("legacy permission lost: %d %v", count, err)
			}
			if err := b.db.Model(&modelRouteAccountModel{}).Where("model_route_id = ? AND account_id = ?", qualityID, credential.ID).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("legacy binding lost: %d %v", count, err)
			}
			for _, table := range []string{"model_routes", "model_route_aliases"} {
				if !b.db.Migrator().HasIndex(table, table+"_legacy_created") {
					t.Fatalf("custom %s index lost", table)
				}
				if dialect == "sqlite" {
					if err := b.db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?", table+"_legacy_trigger").Scan(&count).Error; err != nil || count != 1 {
						t.Fatalf("custom trigger lost: %s %d %v", table, count, err)
					}
				}
				if err := b.db.Table(table).Where("1 = 1").Update("name_source", "invented").Error; err == nil {
					t.Fatalf("%s accepted unrecognized source", table)
				}
			}
		})
	}
}
