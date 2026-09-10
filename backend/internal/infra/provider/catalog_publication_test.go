package provider_test

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

// Public products and wire adapters have different owners. Every fixed product
// must still have an executable Provider implementation, and every registered
// wire text/Web product must have its public policy.
func TestPublishedCatalogMatchesProviderProtocols(t *testing.T) {
	for _, route := range model.CatalogRoutes(account.ProviderWeb) {
		spec, ok := webprovider.Resolve(route.UpstreamModel)
		if !ok || spec.PublicID != model.ExternalPublicID(route.Provider, route.PublicID) || spec.Capability != route.Capability {
			t.Fatalf("Web publication has no matching protocol: %+v %+v", route, spec)
		}
	}
	for _, spec := range webprovider.Catalog() {
		if !model.CatalogSupports(account.ProviderWeb, spec.UpstreamModel, spec.Capability) {
			t.Fatalf("unpublished Web protocol: %+v", spec)
		}
	}
	for _, route := range model.CatalogRoutes(account.ProviderConsole) {
		if route.Capability == model.CapabilityResponses {
			spec, ok := consoleprovider.Resolve(route.UpstreamModel)
			if !ok || spec.PublicID != model.ExternalPublicID(route.Provider, route.PublicID) {
				t.Fatalf("Console publication has no text protocol: %+v", route)
			}
		} else if !consoleprovider.ResolveMedia(route.UpstreamModel, route.Capability) {
			t.Fatalf("Console publication has no media protocol: %+v", route)
		}
	}
	for _, spec := range consoleprovider.Catalog() {
		if !model.CatalogSupports(account.ProviderConsole, spec.UpstreamModel, model.CapabilityResponses) {
			t.Fatalf("unpublished Console protocol: %+v", spec)
		}
	}
}
