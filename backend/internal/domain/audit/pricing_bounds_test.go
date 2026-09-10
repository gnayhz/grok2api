package audit

import (
	"math"
	"math/big"
	"reflect"
	"strconv"
	"testing"
)

// Public estimation and reconstruction must match an independent unbounded
// integer oracle, including sums whose individual products still fit int64.
func TestPricingRangeMatchesExactIntegerArithmetic(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("public media APIs take platform int quantities")
	}
	type contract struct {
		name        string
		rates       []int64
		estimate    func(int64, int64) (PricingResult, bool)
		reconstruct func(int64, int64) (PricingBreakdown, bool)
	}
	var contracts []contract
	for _, tc := range []struct {
		name                           string
		input, cached, output, context int64
		allCached                      bool
	}{
		{"text_standard", 20000, 3000, 60000, 1, false},
		{"text_long", 40000, 6000, 120000, 200001, false},
		{"text_cached", 20000, 3000, 60000, 1, true},
	} {
		rate := tc.input
		if tc.allCached {
			rate = tc.cached
		}
		contracts = append(contracts, contract{tc.name, []int64{rate, tc.output},
			func(a, b int64) (PricingResult, bool) {
				cached := int64(0)
				if tc.allCached {
					cached = a
				}
				return EstimateOfficialCost("grok-4.5", a, cached, b, tc.context)
			},
			func(a, b int64) (PricingBreakdown, bool) {
				cached := int64(0)
				if tc.allCached {
					cached = a
				}
				return ReconstructOfficialCost("grok-4.5", a, cached, b, tc.context, 0, 0, 0)
			},
		})
	}
	for _, tc := range []struct {
		model, resolution, quality, pricing string
		rate                                int64
	}{
		{"grok-imagine-image", "", "", "grok-imagine-image", 200000000},
		{"grok-imagine-image-2.0", "1k", "low", "grok-imagine-image-2.0-low-1k", 400000000},
		{"grok-imagine-image-2.0", "2k", "low", "grok-imagine-image-2.0-low-2k", 600000000},
		{"grok-imagine-image-2.0", "1k", "medium", "grok-imagine-image-2.0-medium-1k", 600000000},
		{"grok-imagine-image-2.0", "2k", "medium", "grok-imagine-image-2.0-medium-2k", 800000000},
		{"grok-imagine-image-quality", "1k", "", "grok-imagine-image-quality-1k", 500000000},
		{"grok-imagine-image-quality", "2k", "", "grok-imagine-image-quality-2k", 700000000},
	} {
		contracts = append(contracts, contract{tc.pricing, []int64{tc.rate},
			func(a, _ int64) (PricingResult, bool) {
				return EstimateOfficialImageCost(tc.model, tc.resolution, tc.quality, int(a))
			},
			func(a, _ int64) (PricingBreakdown, bool) {
				return ReconstructOfficialCost(tc.pricing, 0, 0, 0, 0, 0, a, 0)
			},
		})
	}
	for _, tc := range []struct {
		model, resolution, quality, pricing string
		output, input                       int64
	}{
		{"grok-imagine-image-edit", "1k", "", "grok-imagine-image-edit-1k", 500000000, 100000000},
		{"grok-imagine-image-edit", "2k", "", "grok-imagine-image-edit-2k", 700000000, 100000000},
		{"grok-imagine-image-quality", "1k", "", "grok-imagine-image-quality-edit-1k", 500000000, 100000000},
		{"grok-imagine-image-quality", "2k", "", "grok-imagine-image-quality-edit-2k", 700000000, 100000000},
		{"grok-imagine-image-2.0", "1k", "low", "grok-imagine-image-2.0-edit-low-1k", 400000000, 100000000},
		{"grok-imagine-image-2.0", "2k", "low", "grok-imagine-image-2.0-edit-low-2k", 600000000, 100000000},
		{"grok-imagine-image-2.0", "1k", "medium", "grok-imagine-image-2.0-edit-medium-1k", 600000000, 100000000},
		{"grok-imagine-image-2.0", "2k", "medium", "grok-imagine-image-2.0-edit-medium-2k", 800000000, 100000000},
		{"grok-imagine-image", "1k", "", "grok-imagine-image-edit-lite-1k", 200000000, 20000000},
		{"grok-imagine-image", "2k", "", "grok-imagine-image-edit-lite-2k", 200000000, 20000000},
	} {
		contracts = append(contracts, contract{tc.pricing, []int64{tc.output, tc.input},
			func(a, b int64) (PricingResult, bool) {
				return EstimateOfficialImageEditCost(tc.model, tc.resolution, tc.quality, int(a), int(b))
			},
			func(a, b int64) (PricingBreakdown, bool) {
				return ReconstructOfficialCost(tc.pricing, 0, 0, 0, 0, b, a, 0)
			},
		})
	}
	for _, tc := range []struct {
		model, resolution string
		output, input     int64
	}{
		{"grok-imagine-video", "480p", 500000000, 20000000},
		{"grok-imagine-video", "720p", 700000000, 20000000},
		{"grok-imagine-video-1.5", "480p", 800000000, 100000000},
		{"grok-imagine-video-1.5", "720p", 1400000000, 100000000},
		{"grok-imagine-video-1.5", "1080p", 2500000000, 100000000},
	} {
		contracts = append(contracts, contract{tc.model + "-" + tc.resolution, []int64{tc.output, tc.input},
			func(a, b int64) (PricingResult, bool) {
				return EstimateOfficialVideoCost(tc.model, tc.resolution, int(a), int(b))
			},
			func(a, b int64) (PricingBreakdown, bool) {
				return ReconstructOfficialCost(tc.model+"-"+tc.resolution, 0, 0, 0, 0, b, 0, a)
			},
		})
	}
	contracts = append(contracts, contract{"tts", []int64{150000}, func(a, _ int64) (PricingResult, bool) { return EstimateOfficialTTSCharacterCost(int(a)) }, nil})
	for _, model := range []string{"grok-imagine-image-2.0", "grok-imagine-image-2.0-edit-1k", "grok-imagine-image-2.0-edit-2k"} {
		rates := []int64{400000000}
		if model != "grok-imagine-image-2.0" {
			rates = append(rates, 100000000)
		}
		contracts = append(contracts, contract{"legacy_" + model, rates, nil, func(a, b int64) (PricingBreakdown, bool) { return ReconstructOfficialCost(model, 0, 0, 0, 0, b, a, 0) }})
	}
	for _, tc := range contracts {
		t.Run(tc.name, func(t *testing.T) {
			second := int64(0)
			if len(tc.rates) == 2 {
				second = tc.rates[1]
			}
			limit := (math.MaxInt64 - second) / tc.rates[0]
			quantities := [][2]int64{{1, 1}, {limit, 1}, {limit + 1, 1}, {math.MaxInt64, 1}}
			if second > 0 {
				first := int64(math.MaxInt64) / tc.rates[0]
				quantities = append(quantities, [2]int64{first, (math.MaxInt64-first*tc.rates[0])/second + 1})
			}
			for _, q := range quantities {
				want := new(big.Int)
				for i, rate := range tc.rates {
					want.Add(want, new(big.Int).Mul(big.NewInt(q[i]), big.NewInt(rate)))
				}
				fits := want.IsInt64()
				if tc.estimate != nil {
					got, ok := tc.estimate(q[0], q[1])
					if ok != fits || (ok && got.CostInUSDTicks != want.Int64()) || (!ok && got != (PricingResult{})) {
						t.Fatalf("q=%v estimate=%+v ok=%t exact=%s", q, got, ok, want)
					}
				}
				if tc.reconstruct != nil {
					got, ok := tc.reconstruct(q[0], q[1])
					if ok != fits || (ok && got.CostInUSDTicks != want.Int64()) || (!ok && !reflect.DeepEqual(got, PricingBreakdown{})) {
						t.Fatalf("q=%v breakdown=%+v ok=%t exact=%s", q, got, ok, want)
					}
					if ok {
						sum := new(big.Int)
						for _, c := range got.Components {
							exact := new(big.Int).Mul(big.NewInt(c.Quantity), big.NewInt(c.UnitPriceInUSDTicks))
							if !exact.IsInt64() || c.CostInUSDTicks != exact.Int64() {
								t.Fatalf("invalid component %+v exact=%s", c, exact)
							}
							sum.Add(sum, exact)
						}
						if sum.Cmp(want) != 0 {
							t.Fatalf("component total=%s want=%s", sum, want)
						}
					}
				}
			}
		})
	}
}

func TestBillingExplanationDoesNotReconstructWrappedAmount(t *testing.T) {
	value := Record{InputTokens: math.MaxInt64, OutputTokens: 5, EstimatedCostInUSDTicks: 560000, PricingModel: "grok-4.5", PricingVersion: OfficialPricingAsOf}
	got, ok := value.ExplainBilling()
	if !ok || got.Method != "stored_estimate" || got.TotalInUSDTicks != 560000 || len(got.Components) != 0 {
		t.Fatalf("wrapped historical amount gained a formula: %+v", got)
	}
	value.CostInUSDTicks = 55
	got, ok = value.ExplainBilling()
	if !ok || got.Source != "upstream" || got.TotalInUSDTicks != 55 {
		t.Fatalf("upstream amount changed: %+v", got)
	}
}
