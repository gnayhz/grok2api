package model

import (
	"context"
	"errors"
	"slices"
	"sort"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// ListPublic publishes names and guarantees using the same resolution policy as
// inference. A nil key is the internal unrestricted discovery view.
func (s *Service) ListPublic(ctx context.Context, key *clientkeydomain.Key) ([]modeldomain.PublicModel, error) {
	var tiers []string
	if key != nil {
		scope, valid := clientkeydomain.NormalizeAccountScope(clientkeydomain.AccountScope{Providers: key.ProviderScope, Tiers: key.TierScope})
		if !valid {
			return nil, ErrInvalidFilter
		}
		tiers = scope.Tiers.Values()
		if slices.Contains(tiers, "all") {
			tiers = nil
		}
	}
	snapshot, err := s.models.ReadPublicSnapshot(ctx, tiers)
	if err != nil {
		return nil, err
	}
	return s.describeSnapshot(ctx, snapshot, key)
}

func (s *Service) describeSnapshot(ctx context.Context, snapshot modeldomain.PublicSnapshot, key *clientkeydomain.Key) ([]modeldomain.PublicModel, error) {
	lookup := newSnapshotLookup(snapshot)
	allowAliases := key != nil && key.AllowModelAliases
	visible := func(routes []modeldomain.Route) []modeldomain.Route {
		values := make([]modeldomain.Route, 0, len(routes))
		for _, route := range routes {
			if !lookup.routes[route.ID].ScopeAvailable {
				continue
			}
			if key != nil && (!key.AccountScope().AllowsProvider(route.Provider) || !key.AllowsModel(route.ID)) {
				continue
			}
			values = append(values, route)
		}
		return values
	}
	result := make([]modeldomain.PublicModel, 0, len(snapshot.Routes))
	names := make([]string, 0, len(snapshot.Routes))
	seen := make(map[string]bool, len(snapshot.Routes))
	// Keep the established stable primary-name order. Persisted and fixed aliases
	// remain callable compatibility names; discovery lists primary and opt-in names.
	for _, value := range snapshot.Routes {
		// Only an eligible primary route nominates a discovery name. An
		// unavailable primary must not make another route's persisted alias
		// visible after that route was renamed.
		route := value.Route
		if !route.Enabled || !value.AccountAvailable || !value.ScopeAvailable || !modeldomain.SupportsCapability(route.Provider, route.UpstreamModel, route.Capability) {
			continue
		}
		if key != nil && (!key.AccountScope().AllowsProvider(route.Provider) || !key.AllowsModel(route.ID)) {
			continue
		}
		name := modeldomain.ExternalPublicID(value.Route.Provider, value.Route.PublicID)
		if seen[name] {
			continue
		}
		seen[name] = true
		routes, err := lookup.GetByPublicIDCandidates(ctx, name)
		if err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				return nil, err
			}
			continue
		}
		routes = visible(routes)
		if len(routes) == 0 {
			continue
		}
		names = append(names, name)
		result = append(result, modeldomain.DescribePublicModel(name, routes, ""))
	}
	if !allowAliases {
		return result, nil
	}
	for _, name := range names {
		// Only the established family spellings have dynamic aliases. A custom name
		// retains its actual metadata without silently gaining new alias spellings.
		for _, alias := range modeldomain.PublicReasoningAliasNames(name) {
			if seen[alias] {
				continue
			}
			seen[alias] = true
			// Every configured primary/persisted alias occupies its name, even if it is
			// unavailable to this key. It must never be recreated by expansion.
			if len(lookup.match(alias)) > 0 {
				continue
			}
			routes, effort, err := ResolvePublicRoutes(ctx, lookup, s.providers, alias, true)
			if err != nil {
				if !errors.Is(err, repository.ErrNotFound) {
					return nil, err
				}
				continue
			}
			routes = visible(routes)
			if len(routes) == 0 {
				continue
			}
			// Fixed-reasoning compatibility aliases may be accepted for old clients,
			// but remain hidden because no effort can actually be configured.
			product := modeldomain.DescribePublicModel(alias, routes, effort)
			if effort == "" || !slices.Contains(product.ReasoningLevels, effort) {
				continue
			}
			result = append(result, product)
		}
	}
	return result, nil
}

// snapshotLookup is a request-local adapter for RouteLookup, backed only by the
// two-query snapshot. It shares candidate groups and preferred route identity
// with SQL and is checked against that adapter in the contract tests.
type snapshotLookup struct {
	routes  map[uint64]modeldomain.PublicRoute
	primary map[string][]uint64
	aliases map[string][]uint64
}

func newSnapshotLookup(snapshot modeldomain.PublicSnapshot) *snapshotLookup {
	result := &snapshotLookup{routes: make(map[uint64]modeldomain.PublicRoute, len(snapshot.Routes)), primary: make(map[string][]uint64), aliases: make(map[string][]uint64)}
	for _, value := range snapshot.Routes {
		result.routes[value.Route.ID] = value
		result.primary[value.Route.PublicID] = append(result.primary[value.Route.PublicID], value.Route.ID)
	}
	for _, alias := range snapshot.Aliases {
		result.aliases[alias.Name] = append(result.aliases[alias.Name], alias.RouteID)
	}
	return result
}

func (s *snapshotLookup) match(name string) []modeldomain.PublicRoute {
	for _, group := range modeldomain.NameLookupGroups(name) {
		ids := make(map[uint64]bool)
		for _, candidate := range group.PrimaryIDs {
			for _, id := range s.primary[candidate] {
				ids[id] = true
			}
		}
		for _, candidate := range group.AliasIDs {
			for _, id := range s.aliases[candidate] {
				if _, ok := ids[id]; !ok {
					ids[id] = false
				}
			}
		}
		values := make([]modeldomain.PublicRoute, 0, len(ids))
		for id := range ids {
			if value, ok := s.routes[id]; ok {
				values = append(values, value)
			}
		}
		if len(values) == 0 {
			continue
		}
		providerOrder := account.Providers()
		sort.Slice(values, func(i, j int) bool {
			left, right := values[i].Route, values[j].Route
			if ids[left.ID] != ids[right.ID] {
				return ids[left.ID]
			}
			if left.Provider != right.Provider {
				return slices.Index(providerOrder, left.Provider) < slices.Index(providerOrder, right.Provider)
			}
			return left.ID < right.ID
		})
		return values
	}
	return nil
}

func (s *snapshotLookup) GetByPublicIDCandidates(ctx context.Context, name string) ([]modeldomain.Route, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matched := s.match(name)
	if len(matched) == 0 {
		return nil, repository.ErrNotFound
	}
	candidates := modeldomain.ClassifyCandidates(matched)
	if len(candidates.Routes) == 0 {
		return nil, &repository.ModelRouteUnavailableError{Enabled: candidates.Enabled, Unsupported: candidates.Unsupported}
	}
	return candidates.Routes, nil
}

func (s *snapshotLookup) HasEnabledRouteByPublicID(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, value := range s.match(name) {
		if value.Route.Enabled {
			return true, nil
		}
	}
	return false, nil
}

func (s *snapshotLookup) GetByProviderUpstream(ctx context.Context, provider account.Provider, upstream string) (modeldomain.Route, error) {
	if err := ctx.Err(); err != nil {
		return modeldomain.Route{}, err
	}
	values := make([]modeldomain.Route, 0)
	for _, value := range s.routes {
		route := value.Route
		if route.Provider == provider && route.UpstreamModel == upstream && route.Enabled && value.AccountAvailable && modeldomain.SupportsCapability(route.Provider, route.UpstreamModel, route.Capability) {
			values = append(values, route)
		}
	}
	if len(values) == 0 {
		return modeldomain.Route{}, repository.ErrNotFound
	}
	// SQL's primary-key scan supplies stable ID order before canonical preference.
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	return modeldomain.PreferUpstreamRoute(provider, upstream, values), nil
}
