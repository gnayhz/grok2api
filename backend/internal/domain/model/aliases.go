package model

import (
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// CompatibilityAlias is a hidden, stable public name. Unlike dynamic aliases,
// these names remain accepted when a key disables effort alias discovery.
type CompatibilityAlias struct {
	Alias           string
	PublicModel     string
	Provider        account.Provider
	UpstreamModel   string
	ReasoningEffort string
}

// Effort-suffixed aliases only include levels each Provider/model combination
// actually supports (see domain/model.SupportedReasoningEffortsForProvider).
// No blanket none/low/medium/high/xhigh/max template.
var aliases = []CompatibilityAlias{
	// Compatibility for the temporary PR catalog name. The official quality
	// model itself remains a first-class route.
	consoleAlias("grok-imagine-image-quality-2.0", "grok-imagine-image-quality", "grok-imagine-image-quality", ""),
	consoleAlias("grok-4.3-console", "grok-4.3", "grok-4.3", ""),
	consoleAlias("grok-4.20-0309-reasoning-console", "grok-4.20-0309-reasoning", "grok-4.20-0309-reasoning", ""),
	consoleAlias("grok-4.20-0309-non-reasoning-console", "grok-4.20-0309-non-reasoning", "grok-4.20-0309-non-reasoning", ""),
	consoleAlias("grok-4.20-multi-agent-console", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", ""),
	consoleAlias("grok-4.5-console", "grok-4.5", "grok-4.5", ""),
	consoleAlias("grok-build-console", "grok-build-0.1", "grok-build-0.1", ""),
	consoleAlias("grok-4.3-low", "grok-4.3", "grok-4.3", "low"),
	consoleAlias("grok-4.3-medium", "grok-4.3", "grok-4.3", "medium"),
	consoleAlias("grok-4.3-high", "grok-4.3", "grok-4.3", "high"),
	consoleAlias("grok-4.20-multi-agent-low", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "low"),
	consoleAlias("grok-4.20-multi-agent-medium", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "medium"),
	consoleAlias("grok-4.20-multi-agent-high", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "high"),
	consoleAlias("grok-4.20-multi-agent-xhigh", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "xhigh"),
}

func consoleAlias(alias, publicModel, upstreamModel, effort string) CompatibilityAlias {
	canonical, _ := NormalizePublicID(account.ProviderConsole, publicModel)
	return CompatibilityAlias{
		Alias: alias, PublicModel: canonical, Provider: account.ProviderConsole,
		UpstreamModel: upstreamModel, ReasoningEffort: effort,
	}
}

// CompatibilityAliases returns isolated M05 publication declarations.
func CompatibilityAliases() []CompatibilityAlias {
	return append([]CompatibilityAlias(nil), aliases...)
}

func ResolveCompatibilityAlias(name string) (CompatibilityAlias, bool) {
	for _, value := range aliases {
		if value.Alias == name {
			return value, true
		}
	}
	return CompatibilityAlias{}, false
}

// PublicReasoningAliasNames preserves the literal external name. Qualified
// compatibility syntax is interpreted only by the shared route resolver.
func PublicReasoningAliasNames(name string) []string {
	name = strings.TrimSpace(name)
	_, slug := splitProviderModel(name)
	levels := reasoningEffortsForSlug(slug)
	if len(levels) < 2 {
		return nil
	}
	result := make([]string, 0, len(levels))
	for _, level := range levels {
		result = append(result, name+"-"+level)
	}
	return result
}
