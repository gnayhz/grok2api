package gateway

import (
	"math"
	"testing"
)

func TestWithTotalFallbackFillsFromInputOutput(t *testing.T) {
	usage := Usage{InputTokens: 7, OutputTokens: 5}.WithTotalFallback()
	if usage.TotalTokens != 12 {
		t.Fatalf("total fallback = %d", usage.TotalTokens)
	}
	kept := Usage{InputTokens: 7, OutputTokens: 5, TotalTokens: 3}.WithTotalFallback()
	if kept.TotalTokens != 3 {
		t.Fatalf("reported total must be preserved: %d", kept.TotalTokens)
	}
}

func TestRecomputeAnthropicInputIncludesCacheCreation(t *testing.T) {
	usage := Usage{InputTokens: 20, CachedInputTokens: 5, OutputTokens: 10}.RecomputeAnthropicInput(7)
	if usage.InputTokens != 32 {
		t.Fatalf("anthropic input = %d", usage.InputTokens)
	}
	if usage.TotalTokens != 42 {
		t.Fatalf("anthropic total = %d", usage.TotalTokens)
	}
}

func TestSaturatingUsageSumSaturatesAndIgnoresNonPositive(t *testing.T) {
	if got := SaturatingUsageSum(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("saturated sum = %d", got)
	}
	if got := SaturatingUsageSum(2, -5, 3); got != 5 {
		t.Fatalf("sum ignoring non-positive = %d", got)
	}
}
