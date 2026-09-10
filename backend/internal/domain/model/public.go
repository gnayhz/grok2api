package model

import (
	"slices"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// PublicModel describes the guarantees of the eligible targets for one name.
// PinnedEffort comes from a resolved alias, never from its spelling alone.
type PublicModel struct {
	ID                    string
	CreatedAt             time.Time
	ContextWindow         int
	Description           string
	ImageInput            bool
	ReasoningLevels       []string
	DefaultReasoningLevel string
	ReasoningSupported    bool
	AgentTools            bool
	AgentVisible          bool
	PinnedEffort          string
}

type productMetadata struct {
	contextWindow int
	description   string
	imageInput    bool
}

// Metadata besides reasoning levels. Reasoning levels come from domain/model so aliases and
// Codex catalogs never diverge from the levels each model actually supports.
var productMetadataByUpstream = map[string]productMetadata{
	"grok-4.5":                     {500000, "xAI Grok 4.5 frontier model with reasoning and vision.", true},
	"grok-4.6":                     {500000, "xAI Grok 4.6 frontier model with reasoning and vision.", true},
	"grok-4.3":                     {1000000, "xAI Grok 4.3 high-capacity reasoning model.", true},
	"grok-build-0.1":               {256000, "xAI Grok Build 0.1 coding model.", false},
	"grok-4.20-0309-reasoning":     {2000000, "xAI Grok 4.20 reasoning model.", true},
	"grok-4.20-0309-non-reasoning": {2000000, "xAI Grok 4.20 non-reasoning model.", true},
	"grok-4.20-multi-agent-0309":   {2000000, "xAI Grok 4.20 multi-agent model.", true},
	"grok-3-mini":                  {131072, "xAI Grok 3 Mini model.", false},
	"grok-3-mini-fast":             {131072, "xAI Grok 3 Mini Fast model.", false},
	"grok-composer-2.5-fast":       {200000, "xAI Grok Composer 2.5 model.", false},
}

var defaultProductMetadata = productMetadata{
	contextWindow: 128000,
	description:   "Grok model served via grok2api.",
}

func UpstreamReasoningEfforts(provider account.Provider, upstream string) []string {
	return supportedReasoningEffortsForProviderSlug(provider, upstream)
}

func RouteAcceptsReasoningAlias(route Route, effort string) bool {
	if route.Capability != CapabilityResponses && route.Capability != CapabilityChat {
		return false
	}
	levels := UpstreamReasoningEfforts(route.Provider, route.UpstreamModel)
	return (len(levels) > 1 && levelsContain(levels, effort)) || IsFixedReasoningForProvider(route.Provider, route.UpstreamModel)
}

// DescribePublicModel intersects guarantees across every eligible conversation
// target. Non-conversation capabilities do not make a text target less capable.
func DescribePublicModel(id string, routes []Route, pinnedEffort string) PublicModel {
	result := PublicModel{ID: id, PinnedEffort: pinnedEffort}
	if len(routes) == 0 {
		return result
	}
	result.CreatedAt = routes[0].CreatedAt
	targets := make([]Route, 0, len(routes))
	for _, route := range routes {
		if route.Capability == CapabilityResponses || route.Capability == CapabilityChat {
			targets = append(targets, route)
		}
	}
	for _, route := range routes {
		switch route.Capability {
		case CapabilityImage, CapabilityImageEdit, CapabilityVideo:
		default:
			result.AgentVisible = true
		}
	}
	if len(targets) == 0 {
		targets = routes
	}
	for i, route := range targets {
		metadata, ok := productMetadataByUpstream[route.UpstreamModel]
		if !ok {
			metadata = defaultProductMetadata
		}
		levels := UpstreamReasoningEfforts(route.Provider, route.UpstreamModel)
		reasoning := IsFixedReasoningForProvider(route.Provider, route.UpstreamModel)
		for _, level := range levels {
			reasoning = reasoning || level != ReasoningEffortNone
		}
		tools := route.Provider == account.ProviderBuild && route.Capability == CapabilityResponses
		if i == 0 {
			result.ContextWindow = metadata.contextWindow
			result.Description = metadata.description
			result.ImageInput = metadata.imageInput
			result.ReasoningLevels = levels
			result.ReasoningSupported = reasoning
			result.AgentTools = tools
		} else {
			result.ContextWindow = min(result.ContextWindow, metadata.contextWindow)
			if result.Description != metadata.description {
				result.Description = defaultProductMetadata.description
			}
			result.ImageInput = result.ImageInput && metadata.imageInput
			result.ReasoningLevels = slices.DeleteFunc(result.ReasoningLevels, func(level string) bool { return !slices.Contains(levels, level) })
			result.ReasoningSupported = result.ReasoningSupported && reasoning
			result.AgentTools = result.AgentTools && tools
		}
	}
	result.DefaultReasoningLevel = ReasoningEffortNone
	if len(result.ReasoningLevels) > 0 {
		result.DefaultReasoningLevel = result.ReasoningLevels[0]
	}
	if slices.Contains(result.ReasoningLevels, ReasoningEffortMedium) {
		result.DefaultReasoningLevel = ReasoningEffortMedium
	}
	if pinnedEffort != "" && slices.Contains(result.ReasoningLevels, pinnedEffort) {
		result.ReasoningLevels = []string{pinnedEffort}
		result.DefaultReasoningLevel = pinnedEffort
	}
	return result
}

// PreferUpstreamRoute chooses the stable route identity of a fixed alias.
func PreferUpstreamRoute(provider account.Provider, upstream string, routes []Route) Route {
	preferred := routes[0]
	canonical, err := DefaultDiscoveredRoute(provider, upstream)
	for _, route := range routes {
		if err == nil && route.PublicID == canonical.PublicID {
			return route
		}
		managed := route.Origin == OriginDiscovered || route.Origin == OriginCatalog
		oldManaged := preferred.Origin == OriginDiscovered || preferred.Origin == OriginCatalog
		if (managed && !oldManaged) || (managed == oldManaged && route.ID < preferred.ID) {
			preferred = route
		}
	}
	return preferred
}
