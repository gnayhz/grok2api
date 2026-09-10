package model

import (
	"errors"
	"slices"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

const BuildVideoModel = "grok-imagine-video-1.5"

var ErrUnsupportedCapability = errors.New("模型路由配置了上游模型不支持的能力")

// CapabilityRule is the M05 product contract, independent of account entitlement
// and administrator enablement. ExcludeModels permits remote text model names
// while reserving known media products. All other rules enumerate fixed products.
type CapabilityRule struct {
	Provider       account.Provider
	Capability     Capability
	UpstreamModels []string
	ExcludeModels  bool
}

var capabilityRules = buildCapabilityRules()

func buildCapabilityRules() []CapabilityRule {
	rules := []CapabilityRule{
		{account.ProviderBuild, CapabilityResponses, []string{BuildVideoModel}, true},
		{account.ProviderBuild, CapabilityVideo, []string{BuildVideoModel}, false},
	}
	for _, provider := range []account.Provider{account.ProviderWeb, account.ProviderConsole} {
		for _, capability := range Capabilities() {
			var names []string
			for _, product := range catalogs[provider] {
				if slices.Contains(product.Capabilities, capability) {
					names = append(names, product.UpstreamModel)
				}
			}
			if len(names) > 0 {
				rules = append(rules, CapabilityRule{provider, capability, names, false})
			}
		}
	}
	return rules
}

// CapabilityRules exposes independent values grouped by Provider for storage
// query compilation.
// SQL compiles these decisions; it does not maintain another product catalog.
func CapabilityRules() []CapabilityRule {
	rules := slices.Clone(capabilityRules)
	for index := range rules {
		rules[index].UpstreamModels = slices.Clone(rules[index].UpstreamModels)
	}
	return rules
}

// SupportsCapability accepts normalized upstream identities. Public aliases and
// account bindings cannot change which operation an upstream product implements.
func SupportsCapability(provider account.Provider, upstream string, capability Capability) bool {
	if upstream == "" {
		return false
	}
	for _, rule := range capabilityRules {
		if rule.Provider == provider && rule.Capability == capability && slices.Contains(rule.UpstreamModels, upstream) != rule.ExcludeModels {
			return true
		}
	}
	return false
}

// Availability separates product support from account observations. It never
// changes the administrator's Enabled intent or implies current quota readiness.
type Availability struct {
	CapabilitySupported bool
	CapabilityKnown     bool
	Available           bool
}

func (r Route) Availability() Availability {
	supported := SupportsCapability(r.Provider, r.UpstreamModel, r.Capability)
	bound := len(r.BoundAccountIDs) > 0
	known := !supported || bound || r.SyncedAccounts > 0 || (r.Provider == account.ProviderConsole && (r.Origin == OriginCatalog || r.SupportedAccounts > 0))
	available := supported && r.TotalAccounts > 0 && (r.SupportedAccounts > 0 || (!bound && r.SyncedAccounts < r.TotalAccounts))
	return Availability{supported, known, available}
}
