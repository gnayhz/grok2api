package model

import (
	"fmt"
	"slices"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// CatalogModel defines the public product and its route capabilities. Provider
// adapters own the protocol parameters for executing these products. The first
// capability is the discovery default; discovery never invents a text route for
// a known media product or replaces the complete fixed catalog.
type CatalogModel struct {
	PublicID      string
	UpstreamModel string
	Capabilities  []Capability
}

var catalogs = map[account.Provider][]CatalogModel{
	account.ProviderWeb: {
		{"grok-chat-fast", "grok-chat-fast", []Capability{CapabilityChat}},
		{"grok-chat-auto", "grok-chat-auto", []Capability{CapabilityChat}},
		{"grok-chat-expert", "grok-chat-expert", []Capability{CapabilityChat}},
		{"grok-chat-heavy", "grok-chat-heavy", []Capability{CapabilityChat}},
		{"grok-imagine-image-lite", "grok-imagine-image", []Capability{CapabilityImage}},
		{"grok-imagine-image", "grok-imagine-image-quality", []Capability{CapabilityImage}},
		{"grok-imagine-image-2.0", "grok-imagine-image-2.0", []Capability{CapabilityImage}},
		{"grok-imagine-image-edit", "imagine-image-edit", []Capability{CapabilityImageEdit}},
		{"grok-imagine-video", "grok-imagine-video", []Capability{CapabilityVideo}},
	},
	account.ProviderConsole: {
		{"grok-4.3", "grok-4.3", []Capability{CapabilityResponses}},
		{"grok-4.20-0309-reasoning", "grok-4.20-0309-reasoning", []Capability{CapabilityResponses}},
		{"grok-4.20-0309-non-reasoning", "grok-4.20-0309-non-reasoning", []Capability{CapabilityResponses}},
		{"grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", []Capability{CapabilityResponses}},
		{"grok-4.5", "grok-4.5", []Capability{CapabilityResponses}},
		{"grok-build-0.1", "grok-build-0.1", []Capability{CapabilityResponses}},
		// Preserve this order: reconciliation restores IDs from the historical
		// two-image catalog before inserting the separate 2.0 product.
		{"grok-imagine-image", "grok-imagine-image", []Capability{CapabilityImage, CapabilityImageEdit}},
		{"grok-imagine-image-quality", "grok-imagine-image-quality", []Capability{CapabilityImage, CapabilityImageEdit}},
		{"grok-imagine-image-2.0", "grok-imagine-image-2.0", []Capability{CapabilityImage, CapabilityImageEdit}},
		{"grok-imagine-video", "grok-imagine-video", []Capability{CapabilityVideo}},
		{"grok-imagine-video-1.5", "grok-imagine-video-1.5", []Capability{CapabilityVideo}},
		{"grok-voice-latest", "grok-voice-latest", []Capability{CapabilityRealtime, CapabilityTTS}},
		{"grok-voice-think-fast-2.0", "grok-voice-think-fast-2.0", []Capability{CapabilityRealtime, CapabilityTTS}},
		{"grok-voice-think-fast-1.0", "grok-voice-think-fast-1.0", []Capability{CapabilityRealtime, CapabilityTTS}},
		{"grok-stt", "grok-stt", []Capability{CapabilitySTT}},
	},
}

// CatalogModels returns independent values so adapters cannot mutate publication
// policy. Build remains a remote catalog and has no fixed startup products.
func CatalogModels(provider account.Provider) []CatalogModel {
	values := slices.Clone(catalogs[provider])
	for index := range values {
		values[index].Capabilities = slices.Clone(values[index].Capabilities)
	}
	return values
}

func CatalogSupports(provider account.Provider, upstreamModel string, capability Capability) bool {
	for _, spec := range catalogs[provider] {
		if spec.UpstreamModel == upstreamModel {
			return slices.Contains(spec.Capabilities, capability)
		}
	}
	return false
}

// CatalogDefault gives adapters the public product identity without copying or
// exposing the mutable capability slice. Unknown protocol models are distinct
// from the permissive text defaults used for remote discovery.
func CatalogDefault(provider account.Provider, upstreamModel string) (string, Capability, bool) {
	for _, spec := range catalogs[provider] {
		if spec.UpstreamModel == upstreamModel {
			return spec.PublicID, spec.Capabilities[0], true
		}
	}
	return "", "", false
}

// CatalogRoutes is the desired fixed catalog. Persistence preserves existing
// administrator enablement, bindings, route IDs and compatible names while
// reconciling it. Enabled is only the initial intent for a newly inserted route.
func CatalogRoutes(provider account.Provider) []Route {
	var routes []Route
	for _, spec := range catalogs[provider] {
		publicID, _ := NormalizePublicID(provider, spec.PublicID)
		for _, capability := range spec.Capabilities {
			routes = append(routes, Route{PublicID: publicID, Provider: provider,
				UpstreamModel: spec.UpstreamModel, Capability: capability, Origin: OriginCatalog, Enabled: true})
		}
	}
	return routes
}

// DefaultDiscoveredRoute maps a Provider observation to the initial public
// route. Unknown names retain the existing Provider text default; known fixed
// products share the catalog's public name and primary capability.
func DefaultDiscoveredRoute(provider account.Provider, upstreamModel string) (Route, error) {
	publicName, capability := upstreamModel, CapabilityResponses
	if provider == account.ProviderWeb {
		capability = CapabilityChat
	}
	if provider == account.ProviderBuild && upstreamModel == BuildVideoModel {
		capability = CapabilityVideo
	}
	if name, primary, known := CatalogDefault(provider, upstreamModel); known {
		publicName, capability = name, primary
	}
	publicID, ok := NormalizePublicID(provider, publicName)
	if !ok {
		return Route{}, fmt.Errorf("Provider %s 发现了无效模型 ID %q", provider, publicName)
	}
	return Route{PublicID: publicID, Provider: provider, UpstreamModel: upstreamModel,
		Capability: capability, Origin: OriginDiscovered, Enabled: true}, nil
}

func DiscoveredRoutes(provider account.Provider, upstreamModels []string) ([]Route, error) {
	routes := make([]Route, 0, len(upstreamModels))
	for _, upstreamModel := range upstreamModels {
		route, err := DefaultDiscoveredRoute(provider, upstreamModel)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, nil
}
