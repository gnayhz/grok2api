package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"gorm.io/gorm"
)

func TestModelAliasLegacyKeyMigration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewModelRepository(a)
			routes := make([]model.Route, 2)
			for index := range routes {
				var err error
				routes[index], err = repo.Create(ctx, model.Route{PublicID: "current", Provider: account.ProviderBuild, UpstreamModel: fmt.Sprintf("upstream-%d", index), Capability: model.CapabilityResponses, Enabled: true}, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := a.db.Exec("DROP TABLE model_route_aliases").Error; err != nil {
				t.Fatal(err)
			}
			timeType := "datetime"
			if dialect == "postgres" {
				timeType = "timestamptz"
			}
			if err := a.db.Exec(`CREATE TABLE model_route_aliases (
                alias varchar(255) PRIMARY KEY CHECK (length(trim(alias)) BETWEEN 1 AND 255),
                model_route_id bigint NOT NULL REFERENCES model_routes(id) ON UPDATE CASCADE ON DELETE CASCADE,
                created_at ` + timeType + ` NOT NULL
            )`).Error; err != nil {
				t.Fatal(err)
			}
			when := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
			if err := a.db.Exec("INSERT INTO model_route_aliases(alias, model_route_id, created_at) VALUES (?, ?, ?)", "Build/legacy", routes[0].ID, when).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Exec("CREATE INDEX legacy_alias_created ON model_route_aliases(created_at)").Error; err != nil {
				t.Fatal(err)
			}
			if dialect == "sqlite" {
				if err := a.db.Exec("CREATE TABLE alias_write_probe (alias text)").Error; err != nil {
					t.Fatal(err)
				}
				if err := a.db.Exec("CREATE TRIGGER legacy_alias_probe AFTER INSERT ON model_route_aliases BEGIN INSERT INTO alias_write_probe(alias) VALUES (NEW.alias); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			key := clientKeyModel{ModelScope: "restricted", Name: "alias-key", Prefix: "alias", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "fixture", Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
			if err := a.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Create(&clientKeyModelPermission{ClientKeyID: key.ID, ModelRouteID: routes[0].ID}).Error; err != nil {
				t.Fatal(err)
			}
			failure := errors.New("alias migration rollback fixture")
			hook := "g07_fail_after_alias_upgrade"
			if err := a.db.Callback().Raw().Before("gorm:raw").Register(hook, func(tx *gorm.DB) {
				if strings.Contains(tx.Statement.SQL.String(), "CREATE INDEX IF NOT EXISTS idx_model_route_accounts_account_route") {
					_ = tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			err := a.InitializeSchema(ctx)
			_ = a.db.Callback().Raw().Remove(hook)
			if !errors.Is(err, failure) {
				t.Fatalf("fault did not rollback migration: %v", err)
			}
			// A failed whole-schema transaction must still have the old single key.
			duplicate := modelRouteAliasModel{Alias: "Build/legacy", ModelRouteID: routes[1].ID, CreatedAt: when}
			if err := b.db.Create(&duplicate).Error; err == nil {
				t.Fatal("failed migration left composite key committed")
			}
			for range 2 {
				if err := a.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var saved []modelRouteAliasModel
			if err := b.db.Select(modelAliasNameColumns).Find(&saved).Error; err != nil || len(saved) != 1 || saved[0].ModelRouteID != routes[0].ID || saved[0].NameSource != string(model.NameSourceLegacy) || !saved[0].CreatedAt.Equal(when) {
				t.Fatalf("legacy relationships changed: %+v err %v", saved, err)
			}
			if err := b.db.Create(&duplicate).Error; err != nil {
				t.Fatalf("new composite relation: %v", err)
			}
			if err := b.db.Create(&duplicate).Error; err == nil {
				t.Fatal("duplicate alias/route relation accepted")
			}
			var count int64
			if err := b.db.Model(&clientKeyModelPermission{}).Where("client_key_id = ? AND model_route_id = ?", key.ID, routes[0].ID).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("permission changed %d %v", count, err)
			}
			if !b.db.Migrator().HasIndex("model_route_aliases", "legacy_alias_created") {
				t.Fatal("legacy index lost")
			}
			if dialect == "sqlite" {
				if err := b.db.Table("alias_write_probe").Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("legacy trigger changed: count %d err %v", count, err)
				}
			}
			if err := repo.Delete(ctx, routes[0].ID); err != nil {
				t.Fatal(err)
			}
			if err := b.db.Find(&saved).Error; err != nil || len(saved) != 1 || saved[0].ModelRouteID != routes[1].ID {
				t.Fatalf("alias cascade removed another target: %+v %v", saved, err)
			}
		})
	}
}
