package audit

// BillingExplanation describes an immutable recorded amount. Reconstructing a
// formula never changes the amount or settles the event again.
type BillingExplanation struct {
	Source          string
	Method          string
	Model           string
	Version         string
	Tier            PricingTier
	Components      []PricingComponent
	TotalInUSDTicks int64
}

// ExplainBilling prefers an upstream amount, then the stored official estimate.
// A current rate formula is shown only when it exactly matches the stored amount.
func (v Record) ExplainBilling() (BillingExplanation, bool) {
	if v.CostInUSDTicks > 0 {
		return BillingExplanation{
			Source: "upstream", Method: "upstream_reported", TotalInUSDTicks: v.CostInUSDTicks,
		}, true
	}
	if v.PricingModel == "" {
		return BillingExplanation{}, false
	}
	result := BillingExplanation{
		Source: "official", Method: "stored_estimate", Model: v.PricingModel,
		Version: v.PricingVersion, TotalInUSDTicks: v.EstimatedCostInUSDTicks,
	}
	if v.PricingVersion != OfficialPricingAsOf {
		return result, true
	}
	pricing, ok := ReconstructOfficialCost(v.PricingModel, v.InputTokens, v.CachedInputTokens,
		v.OutputTokens, v.ContextInputTokens, v.MediaInputImages, v.MediaOutputImages, v.MediaOutputSeconds)
	if !ok || pricing.CostInUSDTicks != v.EstimatedCostInUSDTicks {
		return result, true
	}
	result.Method = "official_rates"
	result.Model = pricing.Model
	result.Tier = pricing.Tier
	result.Components = pricing.Components
	return result, true
}

// StreamObservation is a measured audit observation, never an admission or
// quality verdict. Missing throughput is distinct from a measured zero rate.
type StreamObservation struct {
	OutputTokensPerSecond *float64
	DegradeClass          string
}

// ObserveStream retains the historical eligibility and generation-window rules
// for successful streaming records. A terminal burst has a class but no rate.
func (v Record) ObserveStream() StreamObservation {
	result := StreamObservation{}
	if !v.Streaming || v.StatusCode < 200 || v.StatusCode >= 300 || v.ErrorCode != "" || v.FirstTokenMS == nil || v.OutputTokens <= 0 {
		return result
	}
	if ClassifyTerminalBurst(v.OutputTokens, v.ReasoningTokens, *v.FirstTokenMS, v.DurationMS) {
		result.DegradeClass = DegradeClassTerminalBurst
	}
	if v.DurationMS <= *v.FirstTokenMS || GenerationWindowMS(*v.FirstTokenMS, v.DurationMS, v.ReasoningTokens) < DefaultDegradeMinGenMS {
		return result
	}
	throughput := OutputTokensPerSecond(v.OutputTokens, v.ReasoningTokens, *v.FirstTokenMS, v.DurationMS)
	if throughput > 0 {
		result.OutputTokensPerSecond = &throughput
	}
	return result
}
