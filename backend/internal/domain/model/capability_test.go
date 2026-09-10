package model

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"testing"
)

func TestCapabilityRulesAreIndependentAndPreserveRemoteText(t *testing.T) {
	rules := CapabilityRules()
	rules[0].UpstreamModels[0] = "mutated"
	rules[0].ExcludeModels = false
	for _, tc := range []struct {
		up   string
		cap  Capability
		want bool
	}{{"future-text", CapabilityResponses, true}, {"future-text", CapabilityVideo, false}, {BuildVideoModel, CapabilityVideo, true}, {BuildVideoModel, CapabilityResponses, false}, {"", CapabilityResponses, false}} {
		if got := SupportsCapability(account.ProviderBuild, tc.up, tc.cap); got != tc.want {
			t.Fatalf("%s/%s = %v", tc.up, tc.cap, got)
		}
	}
}

func TestAvailabilityKeepsProductAndAccountFactsSeparate(t *testing.T) {
	r := Route{Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: CapabilityImage, Origin: OriginCatalog, Enabled: false, TotalAccounts: 3, SupportedAccounts: 3}
	if a := r.Availability(); !a.CapabilitySupported || !a.CapabilityKnown || !a.Available {
		t.Fatalf("administrator intent changed facts: %+v", a)
	}
	r.Capability = CapabilityResponses
	r.BoundAccountIDs = []uint64{1}
	if a := r.Availability(); a.CapabilitySupported || !a.CapabilityKnown || a.Available {
		t.Fatalf("binding invented capability: %+v", a)
	}
}
