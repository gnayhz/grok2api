package model

import (
	"context"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// RouteLookup is the facts required by the public-name resolution policy. SQL
// serves inference; a request-local snapshot serves public discovery.
type RouteLookup interface {
	GetByPublicIDCandidates(context.Context, string) ([]modeldomain.Route, error)
	GetByProviderUpstream(context.Context, account.Provider, string) (modeldomain.Route, error)
	HasEnabledRouteByPublicID(context.Context, string) (bool, error)
}

// ResolvePublicRoutes preserves configured-name ownership before trying stable
// compatibility names and opt-in effort aliases. It does not authorize a key or
// select an account; those remain M08 and M06 decisions.
func ResolvePublicRoutes(ctx context.Context, lookup RouteLookup, providers provider.Registry, name string, allowAliases bool) ([]modeldomain.Route, string, error) {
	routes, err := lookup.GetByPublicIDCandidates(ctx, name)
	if err == nil {
		return routes, "", nil
	}
	var unavailable *repository.ModelRouteUnavailableError
	if errors.As(err, &unavailable) || !errors.Is(err, repository.ErrNotFound) {
		return nil, "", err
	}
	if alias, ok := modeldomain.ResolveCompatibilityAlias(name); ok && providers != nil {
		if _, registered := providers.Get(alias.Provider); registered {
			route, routeErr := lookup.GetByProviderUpstream(ctx, alias.Provider, alias.UpstreamModel)
			if routeErr != nil {
				return nil, "", resolveMissing(ctx, lookup, alias.PublicModel, routeErr)
			}
			return []modeldomain.Route{route}, alias.ReasoningEffort, nil
		}
	}
	if base, effort, ok := modeldomain.ParseReasoningModelAlias(name); ok && allowAliases {
		routes, resolveErr := lookup.GetByPublicIDCandidates(ctx, base)
		if resolveErr != nil {
			return nil, "", resolveMissing(ctx, lookup, base, resolveErr)
		}
		eligible := make([]modeldomain.Route, 0, len(routes))
		for _, route := range routes {
			if modeldomain.RouteAcceptsReasoningAlias(route, effort) {
				eligible = append(eligible, route)
			}
		}
		if len(eligible) == 0 {
			return nil, "", repository.ErrNotFound
		}
		return eligible, effort, nil
	}
	return nil, "", resolveMissing(ctx, lookup, name, err)
}

func resolveMissing(ctx context.Context, lookup RouteLookup, name string, err error) error {
	var unavailable *repository.ModelRouteUnavailableError
	if errors.As(err, &unavailable) || !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	exists, lookupErr := lookup.HasEnabledRouteByPublicID(ctx, name)
	if lookupErr != nil {
		return lookupErr
	}
	if exists {
		return &repository.ModelRouteUnavailableError{Enabled: true}
	}
	return err
}
