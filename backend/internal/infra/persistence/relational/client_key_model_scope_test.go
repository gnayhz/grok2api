package relational

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestClientKeyModelScopeSurvivesEveryRouteDeletion(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []string{"single", "batch", "catalog"} {
			t.Run(dialect+"/"+kind, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ctx := context.Background()
				models, keys := NewModelRepository(a), NewClientKeyRepository(a)
				var routes []model.Route
				for i := 0; i < 3; i++ {
					route, err := models.Create(ctx, model.Route{PublicID: fmt.Sprintf("route-%d", i), Provider: account.ProviderBuild, UpstreamModel: fmt.Sprintf("upstream-%d", i), Capability: model.CapabilityResponses, Enabled: true}, nil)
					if err != nil {
						t.Fatal(err)
					}
					routes = append(routes, route)
				}
				key, err := keys.Create(ctx, clientkey.Key{Name: "restricted", Prefix: "deletion", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, AllowedModels: []uint64{routes[0].ID, routes[1].ID}})
				if err != nil {
					t.Fatal(err)
				}
				if kind == "catalog" {
					// Catalog reconciliation deletes only its own products.
					if err := a.db.Model(&modelRouteModel{}).Where("id IN ?", []uint64{routes[0].ID, routes[1].ID}).Update("origin", model.OriginCatalog).Error; err != nil {
						t.Fatal(err)
					}
				}
				assertScope := func(want int) {
					t.Helper()
					value, err := NewClientKeyRepository(b).Get(ctx, key.ID)
					if err != nil || value.ModelScope != clientkey.ModelScopeRestricted || len(value.AllowedModels) != want || value.AllowsModel(routes[2].ID) {
						t.Fatalf("scope=%q members=%v err=%v", value.ModelScope, value.AllowedModels, err)
					}
					for _, filter := range []string{"all", "restricted"} {
						values, total, err := NewClientKeyRepository(b).List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 10}, Filter: repository.ClientKeyListFilter{ModelScope: filter}})
						wanted := int64(1)
						if filter == "all" {
							wanted = 0
						}
						if err != nil || total != wanted || int64(len(values)) != wanted {
							t.Fatalf("filter %s: total=%d items=%d err=%v", filter, total, len(values), err)
						}
					}
				}
				if kind == "single" {
					if err := models.Delete(ctx, routes[0].ID); err != nil {
						t.Fatal(err)
					}
					assertScope(1)
					if err := models.Delete(ctx, routes[1].ID); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "batch" {
					if count, err := models.DeleteMany(ctx, []uint64{routes[0].ID, routes[1].ID}); err != nil || count != 2 {
						t.Fatalf("delete %d %v", count, err)
					}
				}
				if kind == "catalog" {
					if err := models.ReplaceProviderRoutes(ctx, account.ProviderBuild, nil); err != nil {
						t.Fatal(err)
					}
				}
				assertScope(0)
				if err := b.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
				assertScope(0)
				// Unrelated management and a later new product cannot widen the key.
				name := "renamed"
				if _, err := keys.Patch(ctx, key.ID, clientkey.ManagementPatch{Name: &name}); err != nil {
					t.Fatal(err)
				}
				assertScope(0)
			})
		}
	}
}

func TestClientKeyModelScopeCommandsAreAtomic(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := seedKeyConsistency(t, a)
			keys := NewClientKeyRepository(a)
			restricted, all := clientkey.ModelScopeRestricted, clientkey.ModelScopeAll
			empty := []uint64{}
			value, err := keys.Patch(ctx, key.ID, clientkey.ManagementPatch{ModelScope: &restricted, AllowedModels: &empty})
			if err != nil || value.ModelScope != restricted || value.AllowsModel(100) {
				t.Fatalf("empty restricted: %+v %v", value, err)
			}
			value, err = keys.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &empty})
			if err != nil || value.ModelScope != all || !value.AllowsModel(100) {
				t.Fatalf("legacy explicit clear: %+v %v", value, err)
			}
			value, err = keys.Patch(ctx, key.ID, clientkey.ManagementPatch{ModelScope: &restricted})
			if err != nil || value.ModelScope != restricted || len(value.AllowedModels) != 0 {
				t.Fatalf("restricted from all: %+v %v", value, err)
			}
			ids := []uint64{key.AllowedModels[0], key.AllowedModels[0]}
			value, err = keys.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &ids})
			if err != nil || value.ModelScope != restricted || len(value.AllowedModels) != 1 {
				t.Fatalf("deduplicated membership: %+v %v", value, err)
			}
			for _, tc := range []struct {
				name  string
				scope *clientkey.ModelScope
				ids   []uint64
			}{
				{"zero", nil, []uint64{0}}, {"unknown", nil, []uint64{999999}}, {"mixed_unknown", nil, []uint64{key.AllowedModels[0], 999999}}, {"all_with_members", &all, key.AllowedModels},
			} {
				t.Run(tc.name, func(t *testing.T) {
					name := "must rollback"
					if _, err := keys.Patch(ctx, key.ID, clientkey.ManagementPatch{Name: &name, ModelScope: tc.scope, AllowedModels: &tc.ids}); !errors.Is(err, repository.ErrInvalidRecord) {
						t.Fatalf("invalid patch err=%v", err)
					}
					actual, err := NewClientKeyRepository(b).Get(ctx, key.ID)
					if err != nil || actual.Name != key.Name || actual.ModelScope != restricted || len(actual.AllowedModels) != 1 || actual.AllowedModels[0] != key.AllowedModels[0] {
						t.Fatalf("partial invalid patch: %+v %v", actual, err)
					}
				})
			}
			value, err = keys.Patch(ctx, key.ID, clientkey.ManagementPatch{ModelScope: &all})
			if err != nil || value.ModelScope != all || len(value.AllowedModels) != 0 {
				t.Fatalf("explicit all didn't clear: %+v %v", value, err)
			}
			if _, err := keys.Create(ctx, clientkey.Key{Name: "invalid-create", Prefix: "invalidcreate", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, AllowedModels: []uint64{999999}}); !errors.Is(err, repository.ErrInvalidRecord) {
				t.Fatalf("create invalid err=%v", err)
			}
			if _, err := keys.GetByPrefix(ctx, "invalidcreate"); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("invalid create left row: %v", err)
			}
		})
	}
}
