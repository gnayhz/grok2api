package audit

import "testing"

var pricingResultSink PricingResult
var pricingBreakdownSink PricingBreakdown
var pricingKnownSink bool

func BenchmarkPricingRange(b *testing.B) {
	b.Run("text", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			pricingResultSink, pricingKnownSink = EstimateOfficialCost("grok-4.5", 1000, 200, 400, 1200)
		}
	})
	b.Run("long_context", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			pricingResultSink, pricingKnownSink = EstimateOfficialCost("grok-4.5", 250000, 100000, 8000, 350000)
		}
	})
	b.Run("image", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			pricingResultSink, pricingKnownSink = EstimateOfficialImageCost("grok-imagine-image-2.0", "2k", "medium", 4)
		}
	})
	b.Run("edit", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			pricingResultSink, pricingKnownSink = EstimateOfficialImageEditCost("grok-imagine-image-2.0", "2k", "medium", 4, 3)
		}
	})
	b.Run("video", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			pricingResultSink, pricingKnownSink = EstimateOfficialVideoCost("grok-imagine-video-1.5", "1080p", 15, 3)
		}
	})
	b.Run("explanation", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			pricingBreakdownSink, pricingKnownSink = ReconstructOfficialCost("grok-4.5", 1000, 200, 400, 1200, 0, 0, 0)
		}
	})
}
