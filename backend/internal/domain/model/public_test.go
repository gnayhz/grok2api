package model

import (
	"reflect"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestPublicMetadataIntersectsEligibleTargetGuarantees(t *testing.T) {
	build := Route{Provider: account.ProviderBuild, PublicID: "Build/shared", UpstreamModel: "grok-4.6", Capability: CapabilityResponses}
	console := Route{Provider: account.ProviderConsole, PublicID: "Console/shared", UpstreamModel: "grok-4.3", Capability: CapabilityResponses}
	product := DescribePublicModel("shared", []Route{build, console}, "")
	if product.ContextWindow != 500000 || !product.ImageInput || product.AgentTools || !product.ReasoningSupported || product.DefaultReasoningLevel != "medium" || !reflect.DeepEqual(product.ReasoningLevels, []string{"low", "medium", "high"}) {
		t.Fatalf("shared guarantees: %+v", product)
	}
	onlyBuild := DescribePublicModel("shared", []Route{build}, "")
	if !onlyBuild.AgentTools || len(onlyBuild.ReasoningLevels) != 4 {
		t.Fatalf("restricted targets lost guarantees: %+v", onlyBuild)
	}
	unknown := build
	unknown.UpstreamModel = "future-model"
	conservative := DescribePublicModel("shared", []Route{build, unknown}, "")
	if conservative.ContextWindow != 128000 || conservative.ImageInput || conservative.ReasoningSupported || len(conservative.ReasoningLevels) != 0 {
		t.Fatalf("unknown target inflated metadata: %+v", conservative)
	}
	// A separate image endpoint under the same name does not degrade the text
	// route's agent metadata, and a real configured suffix is not an alias.
	image := Route{Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: CapabilityImage}
	mixed := DescribePublicModel("grok-4.6-low", []Route{image, build}, "")
	if !mixed.AgentVisible || !mixed.AgentTools || mixed.DefaultReasoningLevel != "medium" || mixed.PinnedEffort != "" || len(mixed.ReasoningLevels) != 4 {
		t.Fatalf("endpoint/name identity: %+v", mixed)
	}
}

func TestPublicDeclarationsDoNotShareMutableSlices(t *testing.T) {
	route := Route{Provider: account.ProviderBuild, UpstreamModel: "grok-4.6", Capability: CapabilityResponses}
	first := DescribePublicModel("custom", []Route{route}, "")
	first.ReasoningLevels[0] = "mutated"
	next := DescribePublicModel("custom", []Route{route}, "")
	if next.ReasoningLevels[0] != "low" {
		t.Fatalf("metadata mutation escaped: %+v", next)
	}
	aliases := CompatibilityAliases()
	aliases[0].Alias = "mutated"
	if _, ok := ResolveCompatibilityAlias("grok-imagine-image-quality-2.0"); !ok {
		t.Fatal("alias declaration mutation escaped")
	}
	if got := PublicReasoningAliasNames("Build/grok-4.6"); !reflect.DeepEqual(got, []string{"Build/grok-4.6-low", "Build/grok-4.6-medium", "Build/grok-4.6-high", "Build/grok-4.6-xhigh"}) {
		t.Fatalf("literal prefix lost: %v", got)
	}
	if got := PublicReasoningAliasNames("grok-4.6-low"); len(got) != 0 {
		t.Fatalf("configured suffix recursively expanded: %v", got)
	}
}

func TestCompatibilityAliasDeclarationsAreCanonicalAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, alias := range CompatibilityAliases() {
		if alias.Alias == "" || seen[alias.Alias] || !IsCanonicalPublicID(alias.Provider, alias.PublicModel) || alias.UpstreamModel == "" {
			t.Fatalf("invalid compatibility declaration: %+v", alias)
		}
		seen[alias.Alias] = true
		public, _, ok := CatalogDefault(alias.Provider, alias.UpstreamModel)
		canonical, valid := NormalizePublicID(alias.Provider, public)
		if !ok || !valid || canonical != alias.PublicModel {
			t.Fatalf("alias target is not a published product: %+v", alias)
		}
		if alias.ReasoningEffort != "" && !RouteAcceptsReasoningAlias(Route{Provider: alias.Provider, UpstreamModel: alias.UpstreamModel, Capability: CapabilityResponses}, alias.ReasoningEffort) {
			t.Fatalf("fixed alias pins unsupported effort: %+v", alias)
		}
	}
}
