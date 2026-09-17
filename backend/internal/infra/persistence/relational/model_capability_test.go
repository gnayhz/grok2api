package relational

import (
	"context"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"reflect"
	"testing"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func capabilityRegistry() provider.Registry {
	return providerimpl.NewRegistry(cli.NewAdapter(cli.Config{}, nil), web.NewAdapter(web.Config{}, nil, nil, nil, nil), console.NewAdapter(console.Config{}, nil, nil, nil))
}

func TestModelCapabilityCreationContract(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, _ := settingsDatabasePair(t, dialect)
			repo := NewModelRepository(db)
			svc := modelapp.NewService(repo, NewAccountRepository(db), nil, capabilityRegistry())
			var accepted int64
			for _, kind := range account.Providers() {
				products := model.CatalogModels(kind)
				if kind == account.ProviderBuild {
					products = []model.CatalogModel{{UpstreamModel: "future-text", Capabilities: []model.Capability{model.CapabilityResponses}}, {UpstreamModel: model.BuildVideoModel, Capabilities: []model.Capability{model.CapabilityVideo}}}
				}
				products = append(products, model.CatalogModel{UpstreamModel: "unknown-product"})
				for _, product := range products {
					for _, cap := range model.Capabilities() {
						valid := false
						for _, want := range product.Capabilities {
							valid = valid || cap == want
						}
						if kind == account.ProviderBuild && product.UpstreamModel == "unknown-product" && cap == model.CapabilityResponses {
							valid = true
						}
						name := fmt.Sprintf("%s/custom-%d-%s", kind.ModelNamespace(), accepted, cap)
						route, err := svc.Create(ctx, modelapp.CreateInput{PublicID: name, Provider: kind, UpstreamModel: kind.ModelNamespace() + "/" + product.UpstreamModel, Capability: cap, Enabled: true})
						if valid {
							if err != nil || model.ExternalPublicID(kind, route.PublicID) != name {
								t.Fatalf("valid %s/%s/%s: %+v %v", kind, product.UpstreamModel, cap, route, err)
							}
							accepted++
						} else if !errors.Is(err, modelapp.ErrInvalidInput) || route.ID != 0 {
							t.Fatalf("invalid %s/%s/%s persisted: %+v %v", kind, product.UpstreamModel, cap, route, err)
						}
					}
				}
			}
			_, total, err := repo.List(ctx, repository.ModelListQuery{})
			if err != nil || total != accepted {
				t.Fatalf("invalid creation changed SQL: total=%d want=%d err=%v", total, accepted, err)
			}
		})
	}
}

func TestModelCapabilityLegacyAvailabilityAndPagination(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, peerDB := settingsDatabasePair(t, dialect)
			repo, peer := NewModelRepository(db), NewModelRepository(peerDB)
			svc := modelapp.NewService(repo, NewAccountRepository(db), nil, capabilityRegistry())
			var validIDs []uint64
			for _, kind := range account.Providers() {
				acct, _, err := NewAccountRepository(db).UpsertByIdentity(ctx, account.Credential{Provider: kind, SourceKey: string(kind), Name: string(kind), EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				names := []string{"unknown-product"}
				for _, product := range model.CatalogModels(kind) {
					names = append(names, product.UpstreamModel)
				}
				if kind == account.ProviderBuild {
					names = append(names, model.BuildVideoModel)
				}
				for _, up := range names {
					for _, cap := range model.Capabilities() {
						name := fmt.Sprintf("%s/%s-%s", kind.ModelNamespace(), up, cap)
						// Direct persistence represents data saved by old releases. It must remain
						// readable and editable, including explicit bindings and enabled intent.
						route, err := repo.Create(ctx, model.Route{Provider: kind, PublicID: name, UpstreamModel: up, Capability: cap, Enabled: true}, []uint64{acct.ID})
						if err != nil {
							t.Fatal(err)
						}
						valid := model.SupportsCapability(kind, up, cap)
						rows, err := peer.GetByPublicIDCandidates(ctx, name)
						if valid {
							if err != nil || len(rows) != 1 || rows[0].ID != route.ID {
								t.Fatalf("valid lookup %+v %v", rows, err)
							}
							validIDs = append(validIDs, route.ID)
						} else {
							var unavailable *repository.ModelRouteUnavailableError
							if !errors.As(err, &unavailable) || !unavailable.Enabled || !unavailable.Unsupported {
								t.Fatalf("invalid lookup %s: %+v %v", name, rows, err)
							}
							got, err := peer.Get(ctx, route.ID)
							if err != nil || !got.Enabled || got.SupportedAccounts != 0 || got.Availability().Available || got.Availability().CapabilitySupported || !reflect.DeepEqual(got.BoundAccountIDs, []uint64{acct.ID}) {
								t.Fatalf("legacy data: %+v %v", got, err)
							}
						}
					}
				}
			}
			enabled, err := peer.ListEnabled(ctx)
			if err != nil || len(enabled) != len(validIDs) {
				t.Fatalf("client list %d want %d: %v", len(enabled), len(validIDs), err)
			}
			seen := map[uint64]bool{}
			for page := 1; page <= len(validIDs)+1; page++ {
				values, total, err := svc.List(ctx, page, 1, "", modelapp.ListFilter{ActiveScope: true, Sort: repository.SortQuery{Field: "publicId", Direction: repository.SortAscending}})
				if err != nil || total != int64(len(validIDs)) {
					t.Fatalf("page %d total %d: %v", page, total, err)
				}
				if page > len(validIDs) {
					if len(values) != 0 {
						t.Fatal("extra page")
					}
					continue
				}
				if len(values) != 1 || seen[values[0].ID] || !values[0].Availability().CapabilitySupported {
					t.Fatalf("short/duplicate/invalid page %d: %+v", page, values)
				}
				seen[values[0].ID] = true
			}
			// Ordering must agree with the displayed support facts, including grouped
			// management pages containing invalid legacy configurations.
			sorted, _, err := svc.List(ctx, 1, 2000, "", modelapp.ListFilter{Sort: repository.SortQuery{Field: "accountSupport", Direction: repository.SortDescending}})
			if err != nil {
				t.Fatal(err)
			}
			for i, r := range sorted {
				if (i < len(validIDs)) != r.Availability().CapabilitySupported {
					t.Fatalf("support sort mismatch index %d: %+v", i, r)
				}
			}
			groups, _, err := svc.ListGroups(ctx, 1, 2000, "", modelapp.ListFilter{Sort: repository.SortQuery{Field: "accountSupport", Direction: repository.SortDescending}})
			if err != nil {
				t.Fatal(err)
			}
			for i, g := range groups {
				if (i < len(validIDs)) != g.Routes[0].Availability().CapabilitySupported {
					t.Fatalf("group support sort mismatch index %d: %+v", i, g)
				}
			}

			for _, kind := range account.Providers() {
				values, err := peer.ListEnabledForScope(ctx, repository.ModelListFilter{Providers: []string{string(kind)}})
				if err != nil {
					t.Fatal(err)
				}
				for _, route := range values {
					if route.Provider != kind || !route.Availability().CapabilitySupported {
						t.Fatalf("scoped list: %+v", route)
					}
				}
			}
		})
	}
}

func TestModelCapabilityMixedPoolKeepsValidTarget(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, _ := settingsDatabasePair(t, dialect)
			repo := NewModelRepository(db)
			acct, _, err := NewAccountRepository(db).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderConsole, SourceKey: "pool", Name: "pool", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			invalid, err := repo.Create(ctx, model.Route{Provider: account.ProviderConsole, PublicID: "pool", UpstreamModel: "grok-imagine-image", Capability: model.CapabilityResponses, Enabled: true}, []uint64{acct.ID})
			if err != nil {
				t.Fatal(err)
			}
			valid, err := repo.Create(ctx, model.Route{Provider: account.ProviderConsole, PublicID: "pool", UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Enabled: true}, []uint64{acct.ID})
			if err != nil {
				t.Fatal(err)
			}
			got, err := repo.GetByPublicIDCandidates(ctx, "pool")
			if err != nil || len(got) != 1 || got[0].ID != valid.ID {
				t.Fatalf("mixed pool target: %+v %v", got, err)
			}
			if _, err := repo.UpdateManyEnabled(ctx, []uint64{valid.ID}, false); err != nil {
				t.Fatal(err)
			}
			_, err = repo.GetByPublicIDCandidates(ctx, "pool")
			var unavailable *repository.ModelRouteUnavailableError
			if !errors.As(err, &unavailable) || !unavailable.Unsupported {
				t.Fatalf("disabled valid target revived invalid one: %v", err)
			}
			current, err := repo.Get(ctx, invalid.ID)
			if err != nil || !current.Enabled || len(current.BoundAccountIDs) != 1 {
				t.Fatalf("invalid intent erased: %+v %v", current, err)
			}
		})
	}
}
