package audit

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// GenerationUsage describes an upstream attempt, not an additional client
// charge or logical request. Selected identifies the attempt whose known usage
// supplies the main record. Other attempts remain attributable to their actual
// account without changing the client billing policy.
type GenerationUsage struct {
	PhysicalID              string      `json:"physical_id"`
	Ordinal                 uint64      `json:"ordinal"`
	AccountID               uint64      `json:"account_id"`
	AccountName             string      `json:"account_name"`
	Model                   string      `json:"model"`
	Selected                bool        `json:"selected"`
	Outcome                 string      `json:"outcome"`
	UsageSource             UsageSource `json:"usage_source"`
	InputTokens             int64       `json:"input_tokens"`
	CachedInputTokens       int64       `json:"cached_input_tokens"`
	CacheCreationTokens     int64       `json:"cache_creation_tokens"`
	OutputTokens            int64       `json:"output_tokens"`
	ReasoningTokens         int64       `json:"reasoning_tokens"`
	TotalTokens             int64       `json:"total_tokens"`
	ContextInputTokens      int64       `json:"context_input_tokens"`
	ContextOutputTokens     int64       `json:"context_output_tokens"`
	NumSourcesUsed          int64       `json:"num_sources_used"`
	NumServerSideToolsUsed  int64       `json:"num_server_side_tools_used"`
	CostInUSDTicks          int64       `json:"cost_in_usd_ticks"`
	EstimatedCostInUSDTicks int64       `json:"estimated_cost_in_usd_ticks"`
	PricingModel            string      `json:"pricing_model"`
	PricingVersion          string      `json:"pricing_version"`
}

// ValidateGenerationUsages bounds the request-owned facts before persistence.
// Reject malformed records rather than silently truncate or combine attempts.
func ValidateGenerationUsages(values []GenerationUsage) error {
	if len(values) > 128 {
		return fmt.Errorf("generation usage exceeds 128 attempts")
	}
	seen := make(map[string]bool, len(values))
	ordinals := make(map[uint64]bool, len(values))
	selected := false
	for _, v := range values {
		if strings.TrimSpace(v.PhysicalID) == "" || len(v.PhysicalID) > 128 || seen[v.PhysicalID] || ordinals[v.Ordinal] || v.Ordinal == 0 || v.AccountID == 0 {
			return fmt.Errorf("generation usage has invalid or duplicate attempt identity")
		}
		seen[v.PhysicalID] = true
		ordinals[v.Ordinal] = true
		if v.Selected && selected {
			return fmt.Errorf("generation usage has multiple selected attempts")
		}
		selected = selected || v.Selected
		if utf8.RuneCountInString(v.AccountName) > 160 || utf8.RuneCountInString(v.Model) > 255 || utf8.RuneCountInString(v.PricingModel) > 100 || len(v.PricingVersion) > 20 {
			return fmt.Errorf("generation usage metadata exceeds storage limits")
		}
		if v.Outcome != "completed" && v.Outcome != "failed" && v.Outcome != "unconfirmed" {
			return fmt.Errorf("generation usage outcome is invalid")
		}
		if v.UsageSource != UsageSourceUpstream && v.UsageSource != UsageSourceEstimated && v.UsageSource != UsageSourceNone {
			return fmt.Errorf("generation usage source is invalid")
		}
		for _, n := range []int64{v.InputTokens, v.CachedInputTokens, v.CacheCreationTokens, v.OutputTokens, v.ReasoningTokens, v.TotalTokens, v.ContextInputTokens, v.ContextOutputTokens, v.NumSourcesUsed, v.NumServerSideToolsUsed, v.CostInUSDTicks, v.EstimatedCostInUSDTicks} {
			if n < 0 {
				return fmt.Errorf("generation usage counters must be nonnegative")
			}
		}
	}
	return nil
}
