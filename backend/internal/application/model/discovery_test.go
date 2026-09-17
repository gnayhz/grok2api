package model

import (
	"context"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"reflect"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func publicSnapshotFixture(routes ...modeldomain.Route) modeldomain.PublicSnapshot {
	snapshot := modeldomain.PublicSnapshot{}
	for i, route := range routes {
		route.ID = uint64(i + 1)
		route.Enabled = true
		route.PublicID, _ = modeldomain.NormalizeExternalPublicID(route.Provider, route.PublicID)
		snapshot.Routes = append(snapshot.Routes, modeldomain.PublicRoute{Route: route, AccountAvailable: true, ScopeAvailable: true})
	}
	return snapshot
}

func TestPublicDiscoveryUsesActualReasoningAndConfiguredIdentity(t *testing.T) {
	snapshot := publicSnapshotFixture(
		modeldomain.Route{PublicID: "grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5", Capability: modeldomain.CapabilityResponses},
		modeldomain.Route{PublicID: "grok-4.6", Provider: account.ProviderBuild, UpstreamModel: "grok-4.6", Capability: modeldomain.CapabilityResponses},
		modeldomain.Route{PublicID: "grok-4.3", Provider: account.ProviderConsole, UpstreamModel: "grok-4.3", Capability: modeldomain.CapabilityResponses},
		modeldomain.Route{PublicID: "grok-4.20-0309-reasoning", Provider: account.ProviderConsole, UpstreamModel: "grok-4.20-0309-reasoning", Capability: modeldomain.CapabilityResponses},
		modeldomain.Route{PublicID: "grok-build-0.1", Provider: account.ProviderBuild, UpstreamModel: "grok-build-0.1", Capability: modeldomain.CapabilityResponses},
		modeldomain.Route{PublicID: "team-coding", Provider: account.ProviderBuild, UpstreamModel: "grok-4.6", Capability: modeldomain.CapabilityResponses},
		modeldomain.Route{PublicID: "grok-4.5-low", Provider: account.ProviderBuild, UpstreamModel: "grok-build-0.1", Capability: modeldomain.CapabilityResponses},
		modeldomain.Route{PublicID: "grok-4.20-multi-agent-0309", Provider: account.ProviderConsole, UpstreamModel: "grok-4.20-multi-agent-0309", Capability: modeldomain.CapabilityResponses},
	)
	service := &Service{providers: providerimpl.NewRegistry(cli.NewAdapter(cli.Config{}, nil), console.NewAdapter(console.Config{}, nil, nil, nil))}
	key := clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll, AllowModelAliases: true}
	products, err := service.describeSnapshot(context.Background(), snapshot, &key)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]modeldomain.PublicModel{}
	for _, product := range products {
		if _, exists := byName[product.ID]; exists {
			t.Fatalf("duplicate %s", product.ID)
		}
		byName[product.ID] = product
	}
	for _, name := range []string{"grok-4.5", "grok-4.5-low", "grok-4.5-medium", "grok-4.5-high", "grok-4.6-low", "grok-4.6-medium", "grok-4.6-high", "grok-4.6-xhigh", "grok-4.3-none", "grok-4.3-low", "grok-4.3-medium", "grok-4.3-high", "grok-4.20-multi-agent-0309-xhigh"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing %s", name)
		}
	}
	for _, name := range []string{"grok-4.5-none", "grok-4.5-xhigh", "grok-4.5-max", "grok-4.3-xhigh", "grok-build-0.1-none", "grok-4.20-0309-reasoning-low", "team-coding-low"} {
		if _, ok := byName[name]; ok {
			t.Errorf("unexpected %s", name)
		}
	}
	renamed := byName["team-coding"]
	if renamed.ContextWindow != 500000 || !reflect.DeepEqual(renamed.ReasoningLevels, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("renamed product: %+v", renamed)
	}
	occupied := byName["grok-4.5-low"]
	if occupied.PinnedEffort != "" || occupied.ContextWindow != 256000 || occupied.DefaultReasoningLevel != "none" || occupied.ReasoningSupported {
		t.Fatalf("configured name reinterpreted: %+v", occupied)
	}
	lookup := newSnapshotLookup(snapshot)
	for _, product := range products {
		routes, effort, err := ResolvePublicRoutes(context.Background(), lookup, service.providers, product.ID, true)
		if err != nil || len(routes) == 0 {
			t.Fatalf("published unresolvable %s: %v", product.ID, err)
		}
		if effort != product.PinnedEffort {
			t.Fatalf("published effort %s: %s vs %s", product.ID, product.PinnedEffort, effort)
		}
	}
}

func TestPublicDiscoveryCannotInventCapabilitiesFromFamilyName(t *testing.T) {
	snapshot := publicSnapshotFixture(modeldomain.Route{PublicID: "grok-4.3", Provider: account.ProviderBuild, UpstreamModel: "future-product", Capability: modeldomain.CapabilityResponses})
	products, err := (&Service{}).describeSnapshot(context.Background(), snapshot, &clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll, AllowModelAliases: true})
	if err != nil || len(products) != 1 || products[0].ContextWindow != 128000 || products[0].DefaultReasoningLevel != "none" {
		t.Fatalf("unknown actual product: %+v %v", products, err)
	}
}

func TestPublicDiscoveryAliasPermissionMatchesFixedResolution(t *testing.T) {
	snapshot := publicSnapshotFixture(
		modeldomain.Route{PublicID: "grok-4.3", Provider: account.ProviderBuild, UpstreamModel: "grok-4.3", Capability: modeldomain.CapabilityResponses},
		modeldomain.Route{PublicID: "grok-4.3", Provider: account.ProviderConsole, UpstreamModel: "grok-4.3", Capability: modeldomain.CapabilityResponses},
	)
	service := &Service{providers: providerimpl.NewRegistry(cli.NewAdapter(cli.Config{}, nil), console.NewAdapter(console.Config{}, nil, nil, nil))}
	for _, tc := range []struct {
		name string
		key  clientkeydomain.Key
		want int
	}{
		{"all", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll, AllowModelAliases: true}, 5},
		{"switch_off", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll}, 1},
		{"build_only", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeRestricted, AllowModelAliases: true, AllowedModels: []uint64{1}}, 2},
		{"console_only", clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll, AllowModelAliases: true, ProviderScope: clientkeydomain.ProviderScopeConsole}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			products, err := service.describeSnapshot(context.Background(), snapshot, &tc.key)
			if err != nil || len(products) != tc.want {
				t.Fatalf("products=%+v err=%v want=%d", products, err, tc.want)
			}
			if tc.name == "build_only" && products[1].ID != "grok-4.3-none" {
				t.Fatalf("Build permission leaked fixed Console alias: %+v", products)
			}
		})
	}
	// The hidden stable alias remains accepted when dynamic discovery is off.
	routes, effort, err := ResolvePublicRoutes(context.Background(), newSnapshotLookup(snapshot), service.providers, "grok-4.3-low", false)
	if err != nil || len(routes) != 1 || routes[0].Provider != account.ProviderConsole || effort != "low" {
		t.Fatalf("stable compatibility alias changed: %+v %s %v", routes, effort, err)
	}
}

func TestPublicDiscoveryPreservesOccupancyBeforeScopeAndAvailability(t *testing.T) {
	for _, state := range []string{"disabled", "no_account", "unsupported", "denied", "scope", "persisted"} {
		t.Run(state, func(t *testing.T) {
			snapshot := publicSnapshotFixture(
				modeldomain.Route{PublicID: "grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5", Capability: modeldomain.CapabilityResponses},
				modeldomain.Route{PublicID: "grok-4.5-low", Provider: account.ProviderBuild, UpstreamModel: "other", Capability: modeldomain.CapabilityResponses},
			)
			key := clientkeydomain.Key{ModelScope: clientkeydomain.ModelScopeAll, AllowModelAliases: true}
			switch state {
			case "disabled":
				snapshot.Routes[1].Route.Enabled = false
			case "no_account":
				snapshot.Routes[1].AccountAvailable = false
			case "unsupported":
				snapshot.Routes[1].Route.Capability = modeldomain.CapabilityVideo
			case "denied":
				key.AllowedModels = []uint64{1}
				key.ModelScope = clientkeydomain.ModelScopeRestricted
			case "scope":
				snapshot.Routes[1].ScopeAvailable = false
			case "persisted":
				snapshot.Routes[1].Route.PublicID = "Build/renamed"
				snapshot.Routes[1].Route.Enabled = false
				snapshot.Aliases = []modeldomain.PersistedAlias{{Name: "Build/grok-4.5-low", RouteID: 2}}
			}
			products, err := (&Service{}).describeSnapshot(context.Background(), snapshot, &key)
			if err != nil {
				t.Fatal(err)
			}
			for _, product := range products {
				if product.ID == "grok-4.5-low" {
					t.Fatalf("occupied %s returned: %+v", state, products)
				}
			}
		})
	}
}

type failedSnapshotRepository struct {
	repository.ModelRepository
	err error
}

func (r failedSnapshotRepository) ReadPublicSnapshot(context.Context, []string) (modeldomain.PublicSnapshot, error) {
	return modeldomain.PublicSnapshot{}, r.err
}
func TestPublicDiscoveryFailureDoesNotPublishPartialCatalog(t *testing.T) {
	failure := fmt.Errorf("snapshot unavailable")
	products, err := (&Service{models: failedSnapshotRepository{err: failure}}).ListPublic(context.Background(), nil)
	if !errors.Is(err, failure) || products != nil {
		t.Fatalf("failure: %+v %v", products, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if products, err := (&Service{}).describeSnapshot(ctx, publicSnapshotFixture(modeldomain.Route{PublicID: "grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5", Capability: modeldomain.CapabilityResponses}), nil); !errors.Is(err, context.Canceled) || products != nil {
		t.Fatalf("canceled: %+v %v", products, err)
	}
}

func TestSnapshotLookupMatchesSQLNameAndTargetContract(t *testing.T) {
	service, _, repo := modelManagementPair(t)
	ctx := context.Background()
	credentials := map[account.Provider]uint64{}
	for _, kind := range account.Providers() {
		value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: kind, Name: string(kind), SourceKey: string(kind), EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		credentials[kind] = value.ID
	}
	var names []string
	var toggled []uint64
	for _, kind := range account.Providers() {
		capability, upstream := modeldomain.CapabilityResponses, "grok-4.5"
		if kind == account.ProviderWeb {
			capability, upstream = modeldomain.CapabilityChat, "grok-chat-fast"
		}
		for _, local := range []string{"shared", kind.ModelNamespace() + "/shared", "renamed"} {
			internal, _ := modeldomain.NormalizeExternalPublicID(kind, local)
			route, err := service.models.Create(ctx, modeldomain.Route{Provider: kind, PublicID: internal, UpstreamModel: upstream, Capability: capability, Enabled: true}, []uint64{credentials[kind]})
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, local, internal, strings.ToLower(internal), " "+local+" ")
			if local == "renamed" {
				next := kind.ModelNamespace() + "/new-name"
				if _, err := service.models.Patch(ctx, route.ID, modeldomain.RoutePatch{PublicID: &next}); err != nil {
					t.Fatal(err)
				}
				names = append(names, "new-name", next)
			} else {
				toggled = append(toggled, route.ID)
			}
		}
	}
	for _, cap := range []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit} {
		if _, err := service.models.Create(ctx, modeldomain.Route{Provider: account.ProviderConsole, PublicID: "Console/images", UpstreamModel: "grok-imagine-image", Capability: cap, Enabled: true}, []uint64{credentials[account.ProviderConsole]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.models.Create(ctx, modeldomain.Route{Provider: account.ProviderBuild, PublicID: "Build/invalid", UpstreamModel: "grok-4.5", Capability: modeldomain.CapabilityVideo, Enabled: true}, []uint64{credentials[account.ProviderBuild]}); err != nil {
		t.Fatal(err)
	}
	names = append(names, "images", "Console/images", "invalid", "Build/invalid", "missing", "Build/missing", "", "Bad/missing")
	for _, enabled := range []bool{true, false} {
		if _, err := service.models.UpdateManyEnabled(ctx, toggled, enabled); err != nil {
			t.Fatal(err)
		}
		snapshot, err := service.models.ReadPublicSnapshot(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		memory := newSnapshotLookup(snapshot)
		for _, name := range names {
			rows, sqlErr := service.models.GetByPublicIDCandidates(ctx, name)
			projected, projectionErr := memory.GetByPublicIDCandidates(ctx, name)
			ids := func(values []modeldomain.Route) []uint64 {
				result := make([]uint64, 0, len(values))
				for _, v := range values {
					result = append(result, v.ID)
				}
				return result
			}
			errorMeaning := func(err error) string {
				var unavailable *repository.ModelRouteUnavailableError
				if errors.As(err, &unavailable) {
					return fmt.Sprintf("unavailable:%t:%t", unavailable.Enabled, unavailable.Unsupported)
				}
				if errors.Is(err, repository.ErrNotFound) {
					return "missing"
				}
				if err != nil {
					return err.Error()
				}
				return ""
			}
			if !reflect.DeepEqual(ids(rows), ids(projected)) || errorMeaning(sqlErr) != errorMeaning(projectionErr) {
				t.Fatalf("name %q enabled=%t: SQL=%v/%v snapshot=%v/%v", name, enabled, ids(rows), sqlErr, ids(projected), projectionErr)
			}
			actual, actualErr := service.models.HasEnabledRouteByPublicID(ctx, name)
			expected, expectedErr := memory.HasEnabledRouteByPublicID(ctx, name)
			if actual != expected || errorMeaning(actualErr) != errorMeaning(expectedErr) {
				t.Fatalf("enabled lookup %q: %t/%v %t/%v", name, actual, actualErr, expected, expectedErr)
			}
		}
		for _, kind := range account.Providers() {
			for _, upstream := range []string{"grok-4.5", "grok-chat-fast", "grok-imagine-image", "missing"} {
				actual, actualErr := service.models.GetByProviderUpstream(ctx, kind, upstream)
				projected, projectedErr := memory.GetByProviderUpstream(ctx, kind, upstream)
				if actual.ID != projected.ID || errors.Is(actualErr, repository.ErrNotFound) != errors.Is(projectedErr, repository.ErrNotFound) {
					t.Fatalf("upstream %s/%s: SQL=%d/%v snapshot=%d/%v", kind, upstream, actual.ID, actualErr, projected.ID, projectedErr)
				}
			}
		}
	}
}

func TestPublicDiscoveryRejectsInvalidRawScopeBeforeStorage(t *testing.T) {
	service := &Service{models: failedSnapshotRepository{err: errors.New("storage must not be read")}}
	for _, key := range []clientkeydomain.Key{{ProviderScope: 128}, {TierScope: 128}} {
		if values, err := service.ListPublic(context.Background(), &key); !errors.Is(err, ErrInvalidFilter) || values != nil {
			t.Fatalf("invalid raw scope normalized into permission: %+v %v", values, err)
		}
	}
}
