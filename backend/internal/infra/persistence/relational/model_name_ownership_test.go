package relational

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCatalogRetirementPreservesAdministratorNames(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"manual_route", "managed_primary", "managed_alias"} {
			t.Run(dialect+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				a, b := settingsDatabasePair(t, dialect)
				accounts, models := NewAccountRepository(a), NewModelRepository(a)
				credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, SourceKey: "manual", Name: "manual", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				service := modelapp.NewService(models, accounts, nil, capabilityRegistry())
				t.Cleanup(func() { _ = service.Close(context.Background()) })
				if err := service.PublishCatalogs(ctx); err != nil {
					t.Fatal(err)
				}
				const retired = "grok-imagine-image-quality-lite"
				bindings := []uint64{credential.ID}
				var product model.Route
				if mode == "manual_route" {
					product, err = service.Create(ctx, modelapp.CreateInput{Provider: account.ProviderWeb, PublicID: retired, UpstreamModel: "grok-chat-fast", Capability: model.CapabilityChat, Enabled: true, AccountIDs: bindings})
				} else {
					product, err = models.GetByPublicID(ctx, "grok-imagine-image")
					if err != nil {
						t.Fatal(err)
					}
					name := retired
					product, err = service.Update(ctx, product.ID, modelapp.UpdateInput{PublicID: &name, AccountIDs: &bindings})
				}
				if err != nil {
					t.Fatal(err)
				}
				if mode != "managed_primary" {
					name := "team-custom"
					if _, err := service.Update(ctx, product.ID, modelapp.UpdateInput{PublicID: &name}); err != nil {
						t.Fatal(err)
					}
				}
				key := clientKeyModel{ModelScope: "restricted", Name: "name-owner", Prefix: "owner", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "fixture", Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
				if err := a.db.Create(&key).Error; err != nil {
					t.Fatal(err)
				}
				if err := a.db.Create(&clientKeyModelPermission{ClientKeyID: key.ID, ModelRouteID: product.ID}).Error; err != nil {
					t.Fatal(err)
				}
				for cycle := 0; cycle < 3; cycle++ {
					if cycle > 0 {
						if err := service.PublishCatalogs(ctx); err != nil {
							t.Fatal(err)
						}
						if err := a.InitializeSchema(ctx); err != nil {
							t.Fatal(err)
						}
					}
					after, err := NewModelRepository(b).GetByPublicID(ctx, retired)
					if err != nil || after.ID != product.ID || after.Origin != product.Origin || after.UpstreamModel != product.UpstreamModel || after.Capability != product.Capability || !after.Enabled {
						t.Fatalf("cycle %d lost name identity: before=%+v after=%+v err=%v", cycle, product, after, err)
					}
					saved, err := models.Get(ctx, product.ID)
					if err != nil || !slices.Equal(saved.BoundAccountIDs, bindings) {
						t.Fatalf("bindings changed: %+v %v", saved, err)
					}
					var count int64
					if err := b.db.Model(&clientKeyModelPermission{}).Where("client_key_id = ? AND model_route_id = ?", key.ID, product.ID).Count(&count).Error; err != nil || count != 1 {
						t.Fatalf("permission changed %d %v", count, err)
					}
				}
				var alias modelRouteAliasModel
				if err := b.db.Where("alias = ? AND model_route_id = ?", "Web/"+retired, product.ID).First(&alias).Error; err != nil || alias.NameSource != string(model.NameSourceManual) {
					t.Fatalf("manual provenance lost: %+v %v", alias, err)
				}
			})
		}
	}
}

func TestCatalogRetirementOnlyRemovesOwnedEdges(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewModelRepository(a)
			if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, model.CatalogRoutes(account.ProviderWeb)); err != nil {
				t.Fatal(err)
			}
			const retired = "Web/grok-imagine-image-quality-lite"
			var manuals []model.Route
			for range 2 {
				value, err := repo.Create(ctx, model.Route{PublicID: retired, Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true}, nil)
				if err != nil {
					t.Fatal(err)
				}
				manuals = append(manuals, value)
			}
			for _, row := range manuals {
				name := "team-custom"
				if _, err := repo.Patch(ctx, row.ID, model.RoutePatch{PublicID: &name}); err != nil {
					t.Fatal(err)
				}
			}
			for _, edge := range []struct {
				upstream string
				source   model.NameSource
			}{{"grok-imagine-image-quality", model.NameSourceGenerated}, {"grok-chat-fast", model.NameSourceLegacy}, {"grok-chat-expert", model.NameSourceGenerated}} {
				var route modelRouteModel
				if err := a.db.Where("provider = ? AND upstream_model = ? AND origin = ?", account.ProviderWeb, edge.upstream, model.OriginCatalog).First(&route).Error; err != nil {
					t.Fatal(err)
				}
				if err := a.db.Create(&modelRouteAliasModel{Alias: retired, ModelRouteID: route.ID, NameSource: string(edge.source)}).Error; err != nil {
					t.Fatal(err)
				}
			}
			var before []modelRouteAliasModel
			if err := a.db.Where("alias = ?", retired).Order("model_route_id").Find(&before).Error; err != nil || len(before) != 5 {
				t.Fatalf("fixture edges %+v %v", before, err)
			}
			quality, err := repo.GetByPublicIDIncludingDisabled(ctx, "grok-imagine-image")
			if err != nil {
				t.Fatal(err)
			}
			want := slices.DeleteFunc(slices.Clone(before), func(edge modelRouteAliasModel) bool { return edge.ModelRouteID == quality.ID })
			for range 2 {
				if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, model.CatalogRoutes(account.ProviderWeb)); err != nil {
					t.Fatal(err)
				}
				var after []modelRouteAliasModel
				if err := b.db.Where("alias = ?", retired).Order("model_route_id").Find(&after).Error; err != nil || !reflect.DeepEqual(want, after) {
					t.Fatalf("retirement changed unrelated relations: want %+v got %+v %v", want, after, err)
				}
			}
		})
	}
}

func TestCatalogManualAliasConflictRollsBackAllChanges(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewModelRepository(a)
			catalog := []model.Route{
				{Provider: account.ProviderConsole, PublicID: "a", UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Enabled: true},
				{Provider: account.ProviderConsole, PublicID: "b", UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true},
				{Provider: account.ProviderConsole, PublicID: "removed", UpstreamModel: "grok-imagine-image-2.0", Capability: model.CapabilityImage, Enabled: true},
			}
			if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, catalog); err != nil {
				t.Fatal(err)
			}
			owner, err := repo.GetByPublicIDIncludingDisabled(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"reserved", "custom"} {
				if _, err := repo.Patch(ctx, owner.ID, model.RoutePatch{PublicID: &name}); err != nil {
					t.Fatal(err)
				}
			}
			var beforeRoutes []modelRouteModel
			var beforeAliases []modelRouteAliasModel
			if err := a.db.Order("id").Find(&beforeRoutes).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Order("alias, model_route_id").Find(&beforeAliases).Error; err != nil {
				t.Fatal(err)
			}
			catalog = catalog[:2]
			catalog[1].PublicID = "reserved"
			if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, catalog); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("catalog took another retained route's manual alias: %v", err)
			}
			var afterRoutes []modelRouteModel
			var afterAliases []modelRouteAliasModel
			if err := b.db.Order("id").Find(&afterRoutes).Error; err != nil || !reflect.DeepEqual(beforeRoutes, afterRoutes) {
				t.Fatalf("conflict committed partial route changes: %+v %v", afterRoutes, err)
			}
			if err := b.db.Order("alias, model_route_id").Find(&afterAliases).Error; err != nil || !reflect.DeepEqual(beforeAliases, afterAliases) {
				t.Fatalf("conflict committed partial alias changes: %+v %v", afterAliases, err)
			}
		})
	}
}

func TestCatalogNameRestoreCarriesRetention(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, source := range []model.NameSource{model.NameSourceManual, model.NameSourceLegacy, model.NameSourceGenerated} {
			t.Run(dialect+"/"+string(source), func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ctx := context.Background()
				repo := NewModelRepository(a)
				old := model.Route{Provider: account.ProviderWeb, PublicID: "grok-imagine-image-quality-lite", UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Origin: model.OriginCatalog, Enabled: true}
				if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{old}); err != nil {
					t.Fatal(err)
				}
				var row modelRouteModel
				if err := a.db.First(&row).Error; err != nil {
					t.Fatal(err)
				}
				// Simulate an already preserved relation, including the pre-metadata case.
				when := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
				if err := a.db.Model(&row).Updates(map[string]any{"public_id": "Web/current", "name_source": model.NameSourceGenerated}).Error; err != nil {
					t.Fatal(err)
				}
				alias := modelRouteAliasModel{Alias: "Web/" + old.PublicID, ModelRouteID: row.ID, NameSource: string(source), CreatedAt: when, ReplacedByCatalog: source == model.NameSourceLegacy}
				if err := a.db.Create(&alias).Error; err != nil {
					t.Fatal(err)
				}
				if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{old}); err != nil {
					t.Fatal(err)
				}
				if err := b.db.First(&row, row.ID).Error; err != nil || row.NameSource != string(source) {
					t.Fatalf("promotion discarded provenance: %+v %v", row, err)
				}
				current := old
				current.PublicID = "grok-imagine-image"
				if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{current}); err != nil {
					t.Fatal(err)
				}
				var aliases []modelRouteAliasModel
				if err := b.db.Where("alias = ?", alias.Alias).Find(&aliases).Error; err != nil {
					t.Fatal(err)
				}
				if source == model.NameSourceGenerated {
					if len(aliases) != 0 {
						t.Fatalf("generated retired edge remains %+v", aliases)
					}
				} else if len(aliases) != 1 || aliases[0].ModelRouteID != row.ID || aliases[0].NameSource != string(source) || aliases[0].ReplacedByCatalog {
					t.Fatalf("restored protection lost on next publication: %+v", aliases)
				}
			})
		}
	}
}
