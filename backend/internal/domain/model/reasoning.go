package model

import (
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// ReasoningEffort is a client-facing reasoning depth level accepted by Grok models.
// Only levels a model actually supports should be advertised or accepted as aliases.
type ReasoningEffort = string

const (
	ReasoningEffortNone   ReasoningEffort = "none"
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
	ReasoningEffortXHigh  ReasoningEffort = "xhigh"
	ReasoningEffortMax    ReasoningEffort = "max"
)

const GrokComposer25Fast = "grok-composer-2.5-fast"

const grokComposerModelPrefix = "grok-composer-"

// reasoningEffortSuffixes is ordered longest-first so "xhigh" wins over "high".
var reasoningEffortSuffixes = []string{
	ReasoningEffortXHigh,
	ReasoningEffortMedium,
	ReasoningEffortHigh,
	ReasoningEffortLow,
	ReasoningEffortNone,
	ReasoningEffortMax,
}

// grokReasoningCapabilities maps external public model IDs to the reasoning levels
// generally accepted by the model family. Provider-specific wire restrictions are
// applied by providerReasoningEffortOverrides:
//   - grok-4.5: low/medium/high (reasoning cannot be disabled; no xhigh/max)
//   - grok-4.6: low/medium/high/xhigh (xhigh is a real upstream effort; max stays guarded)
//   - grok-4.3: none/low/medium/high
//   - grok-4.20-multi-agent: low/medium/high/xhigh (effort controls agent count)
//
// Unknown models default to none-only and never expand into effort aliases.
var grokReasoningCapabilities = map[string][]string{
	"grok-4.5":                     {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-4.6":                     {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh},
	"grok-4.3":                     {ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-build-0.1":               {ReasoningEffortNone},
	"grok-4.20-0309-reasoning":     {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-4.20-0309-non-reasoning": {ReasoningEffortNone},
	"grok-4.20-multi-agent-0309":   {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh},
	"grok-3-mini":                  {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-3-mini-fast":             {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	GrokComposer25Fast:             {ReasoningEffortNone},
}

// IsGrokComposerModel reports whether value belongs to the Composer family.
// Composer uses an isolated upstream conversation and does not accept a
// configurable reasoning effort, including when addressed through Build/.
func IsGrokComposerModel(value string) bool {
	return strings.HasPrefix(strings.ToLower(externalModelSlug(value)), grokComposerModelPrefix)
}

// The Console reasoning variant has a fixed reasoning mode: it produces reasoning
// but rejects the reasoningEffort parameter. Build may expose a different contract
// for the same model slug, so this restriction must remain provider-scoped.
var providerReasoningEffortOverrides = map[account.Provider]map[string][]string{
	account.ProviderConsole: {
		"grok-4.20-0309-reasoning": {},
	},
}

var providerFixedReasoningModels = map[account.Provider]map[string]struct{}{
	account.ProviderConsole: {
		"grok-4.20-0309-reasoning": {},
	},
}

// SupportedReasoningEfforts returns the reasoning levels a public model ID actually supports.
// Provider prefixes are stripped; unknown models only advertise "none".
// Effort-suffixed aliases (e.g. grok-4.5-low) inherit the base model's levels.
func SupportedReasoningEfforts(publicModel string) []string {
	providerValue, slug := splitProviderModel(publicModel)
	if base, _, ok := parseReasoningModelAliasSlug(slug); ok {
		slug = base
	}
	if providerValue != "" {
		return supportedReasoningEffortsForProviderSlug(providerValue, slug)
	}
	return reasoningEffortsForSlug(slug)
}

// SupportedReasoningEffortsForProvider returns only effort values accepted by
// the selected Provider's wire contract. An empty result means the model may
// reason intrinsically but does not expose a configurable effort parameter.
func SupportedReasoningEffortsForProvider(providerValue account.Provider, publicModel string) []string {
	slug := externalModelSlug(publicModel)
	if base, _, ok := parseReasoningModelAliasSlug(slug); ok {
		slug = base
	}
	return supportedReasoningEffortsForProviderSlug(providerValue, slug)
}

func supportedReasoningEffortsForProviderSlug(providerValue account.Provider, slug string) []string {
	if overrides, ok := providerReasoningEffortOverrides[providerValue]; ok {
		if levels, exists := overrides[slug]; exists {
			return append([]string(nil), levels...)
		}
	}
	return reasoningEffortsForSlug(slug)
}

// SupportsReasoningEffort reports whether publicModel accepts the given effort level.
func SupportsReasoningEffort(publicModel, effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	for _, level := range SupportedReasoningEfforts(publicModel) {
		if level == effort {
			return true
		}
	}
	return false
}

// SupportsReasoningForProvider reports whether a model produces reasoning even
// when its Provider does not accept an explicit effort parameter.
func SupportsReasoningForProvider(providerValue account.Provider, publicModel string) bool {
	if IsFixedReasoningForProvider(providerValue, publicModel) {
		return true
	}
	for _, effort := range SupportedReasoningEffortsForProvider(providerValue, publicModel) {
		if effort != ReasoningEffortNone {
			return true
		}
	}
	return false
}

// IsFixedReasoningForProvider reports whether the model reasons intrinsically
// while rejecting explicit effort controls on the selected Provider.
func IsFixedReasoningForProvider(providerValue account.Provider, publicModel string) bool {
	slug := externalModelSlug(publicModel)
	if fixed, ok := providerFixedReasoningModels[providerValue]; ok {
		if _, exists := fixed[slug]; exists {
			return true
		}
	}
	return false
}

// 各 Provider 的默认推理档位由其目录事实决定(如 Console catalog 的
// DefaultReasoningEffort 字段);domain 不维护集中缺省规则。

func reasoningEffortsForSlug(slug string) []string {
	if levels, ok := grokReasoningCapabilities[slug]; ok {
		return append([]string(nil), levels...)
	}
	return []string{ReasoningEffortNone}
}

// 档位别名的唯一展开规则在 aliases.go 的 PublicReasoningAliasNames。

// ParseReasoningModelAlias splits names like "grok-4.5-low" into base model + effort
// when the model family defines that suffix. Provider-specific wire handling is
// intentionally deferred so fixed-reasoning Providers can preserve compatibility
// by accepting the alias and dropping the unsupported effort before forwarding.
// Base retains any provider prefix present on the input (e.g. Build/grok-4.5).
func ParseReasoningModelAlias(publicModel string) (baseModel, effort string, ok bool) {
	name := strings.TrimSpace(publicModel)
	if name == "" {
		return "", "", false
	}
	providerPrefix := ""
	local := name
	for _, candidate := range account.Providers() {
		prefix := candidate.ModelNamespace() + "/"
		if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			providerPrefix = name[:len(prefix)]
			local = strings.TrimSpace(name[len(prefix):])
			break
		}
	}
	baseLocal, effort, ok := parseReasoningModelAliasSlug(local)
	if !ok {
		return "", "", false
	}
	return providerPrefix + baseLocal, effort, true
}

// parseReasoningModelAliasSlug parses an unprefixed model slug only.
// It looks up capabilities directly to avoid recursion with SupportedReasoningEfforts.
func parseReasoningModelAliasSlug(slug string) (base, effort string, ok bool) {
	name := strings.TrimSpace(slug)
	if name == "" {
		return "", "", false
	}
	for _, level := range reasoningEffortSuffixes {
		suffix := "-" + level
		if len(name) <= len(suffix) {
			continue
		}
		if !strings.EqualFold(name[len(name)-len(suffix):], suffix) {
			continue
		}
		base = strings.TrimSpace(name[:len(name)-len(suffix)])
		if base == "" {
			continue
		}
		// Aliases only exist when the base model has multiple controllable levels;
		// single-level models (e.g. grok-build-0.1 → none only) keep the base name.
		levels := reasoningEffortsForSlug(base)
		if len(levels) < 2 || !levelsContain(levels, level) {
			continue
		}
		return base, level, true
	}
	return "", "", false
}

func levelsContain(levels []string, effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	for _, level := range levels {
		if level == effort {
			return true
		}
	}
	return false
}

func externalModelSlug(publicModel string) string {
	_, slug := splitProviderModel(publicModel)
	return slug
}

func splitProviderModel(publicModel string) (account.Provider, string) {
	value := strings.TrimSpace(publicModel)
	if value == "" {
		return "", ""
	}
	for _, providerValue := range account.Providers() {
		prefix := providerValue.ModelNamespace() + "/"
		if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			return providerValue, strings.TrimSpace(value[len(prefix):])
		}
	}
	return "", value
}
