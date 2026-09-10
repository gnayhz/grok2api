package model

import (
	"slices"
	"strings"
)

// PublicSnapshot contains request-local routing facts from one database snapshot.
// Aliases contains active relationships; catalog-replaced historical edges are
// retained by persistence but cannot become routing or permission candidates.
// Unavailable and disabled rows still occupy their configured names.
type PublicSnapshot struct {
	Routes  []PublicRoute
	Aliases []PersistedAlias
}

type PublicRoute struct {
	Route            Route
	AccountAvailable bool
	ScopeAvailable   bool
}

type PersistedAlias struct {
	Name    string
	RouteID uint64
}

// AvailableCandidates keeps configured identity separate from runtime eligibility.
// Both SQL lookup and snapshot lookup use this classification.
type AvailableCandidates struct {
	Routes      []Route
	Enabled     bool
	Unsupported bool
}

func ClassifyCandidates(facts []PublicRoute) AvailableCandidates {
	result := AvailableCandidates{Routes: make([]Route, 0, len(facts))}
	supported := false
	for _, fact := range facts {
		route := fact.Route
		if !route.Enabled {
			continue
		}
		result.Enabled = true
		if !SupportsCapability(route.Provider, route.UpstreamModel, route.Capability) {
			continue
		}
		supported = true
		if fact.AccountAvailable {
			result.Routes = append(result.Routes, route)
		}
	}
	result.Unsupported = result.Enabled && !supported
	return result
}

// NameLookupGroup is one precedence tier. The original literal alias is checked
// only in the final tier, so it cannot bypass a configured prefixed public name.
type NameLookupGroup struct {
	PrimaryIDs []string
	AliasIDs   []string
}

func NameLookupGroups(name string) []NameLookupGroup {
	groups := PublicIDCandidateGroups(name)
	name = strings.TrimSpace(name)
	if len(groups) == 0 {
		return []NameLookupGroup{{AliasIDs: []string{name}}}
	}
	result := make([]NameLookupGroup, 0, len(groups))
	for i, primary := range groups {
		aliases := append([]string(nil), primary...)
		if i == len(groups)-1 && !slices.Contains(aliases, name) {
			aliases = append(aliases, name)
		}
		result = append(result, NameLookupGroup{PrimaryIDs: primary, AliasIDs: aliases})
	}
	return result
}
