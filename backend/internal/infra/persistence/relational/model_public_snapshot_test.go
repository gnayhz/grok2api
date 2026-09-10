package relational

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestPublicModelSnapshotMatchesInferenceAndScope(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peerDB := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			writer, reader := NewModelRepository(db), NewModelRepository(peerDB)
			accounts := NewAccountRepository(db)
			bound := map[account.Provider]uint64{}
			for _, kind := range account.Providers() {
				credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: kind, SourceKey: string(kind), Name: string(kind), EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive, WebTier: account.WebTierBasic})
				if err != nil {
					t.Fatal(err)
				}
				bound[kind] = credential.ID
			}
			create := func(kind account.Provider, name, upstream string, cap model.Capability) model.Route {
				route, err := writer.Create(ctx, model.Route{Provider: kind, PublicID: kind.ModelNamespace() + "/" + name, UpstreamModel: upstream, Capability: cap, Enabled: true}, []uint64{bound[kind]})
				if err != nil {
					t.Fatal(err)
				}
				return route
			}
			build := create(account.ProviderBuild, "grok-4.3", "grok-4.3", model.CapabilityResponses)
			console := create(account.ProviderConsole, "grok-4.3", "grok-4.3", model.CapabilityResponses)
			image := create(account.ProviderConsole, "images", "grok-imagine-image", model.CapabilityImage)
			create(account.ProviderConsole, "images", "grok-imagine-image", model.CapabilityImageEdit)
			create(account.ProviderWeb, "web-basic", "grok-chat-fast", model.CapabilityChat)
			renamed := create(account.ProviderBuild, "grok-4.5", "grok-4.5", model.CapabilityResponses)
			occupied := create(account.ProviderBuild, "grok-4.5-low", "grok-build-0.1", model.CapabilityResponses)
			disabled := false
			if _, err := writer.Patch(ctx, occupied.ID, model.RoutePatch{Enabled: &disabled}); err != nil {
				t.Fatal(err)
			}
			literal := create(account.ProviderBuild, "Build/grok-4.5-medium", "grok-build-0.1", model.CapabilityResponses)
			literalName := "Build/literal-current"
			if _, err := writer.Patch(ctx, literal.ID, model.RoutePatch{PublicID: &literalName, Enabled: &disabled}); err != nil {
				t.Fatal(err)
			}
			// Renaming preserves the old primary name as a persisted alias.
			changed := "Build/team-coding"
			if _, err := writer.Patch(ctx, renamed.ID, model.RoutePatch{PublicID: &changed}); err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int32
			const countCallback = "g12_count_public_queries"
			if err := peerDB.db.Callback().Query().After("gorm:query").Register(countCallback, func(*gorm.DB) { reads.Add(1) }); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = peerDB.db.Callback().Query().Remove(countCallback) })
			registry := capabilityRegistry()
			service := modelapp.NewService(reader, NewAccountRepository(peerDB), nil, registry)
			cases := []struct {
				name   string
				key    clientkeydomain.Key
				has    []string
				absent []string
			}{
				{"all", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll, AllowModelAliases: true}, []string{"grok-4.3", "grok-4.3-none", "grok-4.3-low", "grok-4.3-medium", "grok-4.3-high", "images", "web-basic", "team-coding"}, []string{"grok-4.5-low", "grok-4.5-medium", "grok-4.5-high", "Build/grok-4.5-medium"}},
				{"build_permission", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeRestricted, AllowModelAliases: true, AllowedModels: []uint64{build.ID, renamed.ID}}, []string{"grok-4.3", "grok-4.3-none", "team-coding"}, []string{"grok-4.3-low", "images", "web-basic"}},
				{"console_permission", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeRestricted, AllowModelAliases: true, AllowedModels: []uint64{console.ID, image.ID}}, []string{"grok-4.3", "grok-4.3-none", "grok-4.3-low", "images"}, []string{"team-coding", "web-basic"}},
				{"web_scope", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll, AllowModelAliases: true, ProviderScope: clientkeydomain.ProviderScopeWeb, TierScope: clientkeydomain.TierScopeFree}, []string{"web-basic"}, []string{"grok-4.3", "images", "team-coding"}},
				{"web_wrong_tier", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll, AllowModelAliases: true, ProviderScope: clientkeydomain.ProviderScopeWeb, TierScope: clientkeydomain.TierScopeSuper}, nil, []string{"web-basic", "grok-4.3", "images", "team-coding"}},
				{"no_alias", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll}, []string{"grok-4.3", "images", "web-basic", "team-coding"}, []string{"grok-4.3-none", "grok-4.3-low"}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					reads.Store(0)
					products, err := service.ListPublic(ctx, &tc.key)
					if count := reads.Load(); count != 2 {
						t.Fatalf("public discovery used %d queries, want two batch reads", count)
					}
					if err != nil {
						t.Fatal(err)
					}
					ids := make([]string, 0, len(products))
					for _, product := range products {
						if slices.Contains(ids, product.ID) {
							t.Fatalf("duplicated public name %s", product.ID)
						}
						ids = append(ids, product.ID)
						routes, effort, err := modelapp.ResolvePublicRoutes(ctx, reader, registry, product.ID, tc.key.AllowModelAliases)
						if err != nil || len(routes) == 0 {
							t.Fatalf("listed name cannot resolve: %s %v", product.ID, err)
						}
						allowed := false
						for _, route := range routes {
							allowed = allowed || (tc.key.AccountScope().AllowsProvider(route.Provider) && tc.key.AllowsModel(route.ID))
						}
						if !allowed || effort != product.PinnedEffort {
							t.Fatalf("publication diverged from inference: %+v %+v %q", product, routes, effort)
						}
						if product.ID == "team-coding" && (product.ContextWindow != 500000 || len(product.ReasoningLevels) != 3) {
							t.Fatalf("renamed product lost metadata: %+v", product)
						}
						if product.ID == "grok-4.3" && product.AgentTools != (tc.name == "build_permission") {
							t.Fatalf("shared target tool guarantee: %+v", product)
						}
					}
					for _, id := range tc.has {
						if !slices.Contains(ids, id) {
							t.Errorf("missing %s from %v", id, ids)
						}
					}
					for _, id := range tc.absent {
						if slices.Contains(ids, id) {
							t.Errorf("unexpected %s in %v", id, ids)
						}
					}
				})
			}
			// Literal qualified names stay occupied through a rename and disable, and
			// cannot fall back to the otherwise valid dynamic Build alias.
			_, _, err := modelapp.ResolvePublicRoutes(ctx, reader, registry, "Build/grok-4.5-medium", true)
			var unavailable *repository.ModelRouteUnavailableError
			if !errors.As(err, &unavailable) || unavailable.Enabled {
				t.Fatalf("literal alias identity lost: %v", err)
			}
		})
	}
}

func TestPublicModelSnapshotKeepsOneCommittedView(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			repo, writer := NewModelRepository(db), NewModelRepository(peer)
			route, err := repo.Create(ctx, model.Route{Provider: account.ProviderBuild, PublicID: "Build/old-name", UpstreamModel: "grok-4.5", Capability: model.CapabilityResponses, Enabled: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			var interleaved atomic.Bool
			const callback = "g12_write_between_public_snapshot_reads"
			if err := db.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != "model_routes" || !interleaved.CompareAndSwap(false, true) {
					return
				}
				go func() {
					name := "Build/new-name"
					enabled := false
					_, err := writer.Patch(ctx, route.ID, model.RoutePatch{PublicID: &name, Enabled: &enabled})
					done <- err
				}()
				if dialect == "postgres" {
					select {
					case err := <-done:
						if err != nil {
							tx.AddError(err)
						}
						done <- err
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.db.Callback().Query().Remove(callback) })
			first, err := repo.ReadPublicSnapshot(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !interleaved.Load() {
				t.Fatal("concurrent writer did not cross read boundary")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if len(first.Routes) != 1 || first.Routes[0].Route.PublicID != "Build/old-name" || !first.Routes[0].Route.Enabled || len(first.Aliases) != 0 {
				t.Fatalf("mixed first snapshot: %+v", first)
			}
			next, err := repo.ReadPublicSnapshot(ctx, nil)
			if err != nil || len(next.Routes) != 1 || next.Routes[0].Route.PublicID != "Build/new-name" || next.Routes[0].Route.Enabled || !reflect.DeepEqual(next.Aliases, []model.PersistedAlias{{Name: "Build/old-name", RouteID: route.ID}}) {
				t.Fatalf("next snapshot missed commit: %+v %v", next, err)
			}
			canceled, stop := context.WithCancel(ctx)
			stop()
			if value, err := repo.ReadPublicSnapshot(canceled, nil); !errors.Is(err, context.Canceled) || len(value.Routes) > 0 {
				t.Fatalf("canceled snapshot: %+v %v", value, err)
			}
			failure := errors.New("alias read failed")
			const failCallback = "g12_fail_public_alias_read"
			if err := db.db.Callback().Query().Before("gorm:query").Register(failCallback, func(tx *gorm.DB) {
				if tx.Statement.Table == "model_route_aliases" {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			value, err := repo.ReadPublicSnapshot(ctx, nil)
			_ = db.db.Callback().Query().Remove(failCallback)
			if !errors.Is(err, failure) || len(value.Routes) > 0 || len(value.Aliases) > 0 {
				t.Fatalf("partial snapshot escaped: %+v %v", value, err)
			}
			// Both cancellation and failure release the transaction; the next reader works.
			if _, err := repo.ReadPublicSnapshot(ctx, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
