package gateway

import "testing"

// TestClassifyQualityHoldOversizedFailOpen 补齐决策表缺失格（round 25，
// HARDENING A-P1-3）：OversizedLine 优先于扣留判定——无法解析的流不猜
// 质量，即使有大量可见输出也 fail-open 放行（与缓冲上限语义一致）。
func TestClassifyQualityHoldOversizedFailOpen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sig  QualityStreamSignals
		want QualityVerdict
	}{
		{
			name: "oversized alone waits",
			sig:  QualityStreamSignals{OversizedLine: true},
			want: QualityWait,
		},
		{
			name: "oversized with large visible withholds",
			sig:  QualityStreamSignals{OversizedLine: true, VisibleTokens: 500, Terminal: true},
			want: QualityWithhold,
		},
		{
			name: "oversized does not outrank real thinking",
			sig:  QualityStreamSignals{OversizedLine: true, HasThinking: true, VisibleTokens: 4},
			want: QualityDeliver,
		},
		{
			name: "zero-output oversized waits",
			sig:  QualityStreamSignals{OversizedLine: true, VisibleTokens: 0},
			want: QualityWait,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyQualityHold(tc.sig, 32); got != tc.want {
				t.Fatalf("ClassifyQualityHold(%+v) = %s, want %s", tc.sig, got, tc.want)
			}
		})
	}
}
