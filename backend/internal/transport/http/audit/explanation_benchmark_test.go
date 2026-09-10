package audit

import (
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"testing"
)

var explanationBenchmarkSink auditResponse

// The same fixture runs against the old HTTP rules and the owned projections.
// This isolates DTO interpretation from database and HTTP transport latency.
func BenchmarkAuditExplanation(b *testing.B) {
	first := int64(250)
	values := []struct {
		name  string
		value auditdomain.Record
	}{
		{"official", auditdomain.Record{InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50, ContextInputTokens: 100, EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "2026-08-13", StatusCode: 200, Streaming: true, FirstTokenMS: &first, DurationMS: 1250}},
		{"upstream", auditdomain.Record{CostInUSDTicks: 2_500_000, StatusCode: 200, Streaming: true, FirstTokenMS: &first, DurationMS: 1250, OutputTokens: 80}},
		{"historical", auditdomain.Record{EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "2025-01-01", StatusCode: 200}},
	}
	for _, tc := range values {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				explanationBenchmarkSink = newAuditResponse(tc.value)
			}
		})
	}
}
