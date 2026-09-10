package model

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestCatalogDoesNotExposeMutablePolicy(t *testing.T) {
	values := CatalogModels(account.ProviderConsole)
	values[0].PublicID = "mutated"
	values[0].Capabilities[0] = CapabilityImage
	name, capability, known := CatalogDefault(account.ProviderConsole, "grok-4.3")
	if !known || name != "grok-4.3" || capability != CapabilityResponses {
		t.Fatalf("adapter changed shared policy: %s %s %v", name, capability, known)
	}
}

func TestUnknownDiscoveryRetainsProviderDefaults(t *testing.T) {
	for _, provider := range account.Providers() {
		capability := CapabilityResponses
		if provider == account.ProviderWeb {
			capability = CapabilityChat
		}
		route, err := DefaultDiscoveredRoute(provider, "future-product")
		if err != nil || route.PublicID != provider.ModelNamespace()+"/future-product" || route.UpstreamModel != "future-product" || route.Capability != capability || route.Origin != OriginDiscovered || !route.Enabled {
			t.Fatalf("unknown discovery: %+v %v", route, err)
		}
		if _, _, known := CatalogDefault(provider, "future-product"); known {
			t.Fatal("unknown model became a fixed protocol")
		}
		if _, err := DiscoveredRoutes(provider, []string{"valid", ""}); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
}
