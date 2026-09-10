package web

import (
	"context"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

func webGenerationUsage(parsed *parsedChat) jsonpeek.TokenUsage {
	reasoning := estimateTokens(parsed.Reasoning.String())
	output := estimateTokens(parsed.Text.String()) + reasoning + estimateToolCallTokens(parsed.ToolCalls)
	return jsonpeek.TokenUsage{Found: true, Input: parsed.InputTokens, Output: output, Total: parsed.InputTokens + output,
		Reasoning: reasoning, Sources: int64(len(parsed.SearchSources)), ServerTools: parsed.ServerTools}
}

// Web has no authoritative token counters. Preserve the adapter's estimate
// once native output is observed, before local archive/state/delivery can fail.
// The registry keeps these observations explicitly labelled as estimated.
func observeWebGeneration(ctx context.Context, physicalID string, parsed *parsedChat) {
	infraegress.ObservePhysicalGeneration(ctx, physicalID, parsed.GenerationOutcome)
	if parsed.GenerationOutcome == "" && parsed.Text.Len() == 0 && parsed.Reasoning.Len() == 0 && len(parsed.ToolCalls) == 0 && parsed.ServerTools == 0 {
		return
	}
	infraegress.ObserveCanonicalPhysicalUsage(ctx, physicalID, webGenerationUsage(parsed))
}
