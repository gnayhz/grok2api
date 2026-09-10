package gateway

import "testing"

// TestClassifyQualityHoldDecisionTable 锁定零延迟状态机的核心决策表
// （蓝图 §四）：思考增量放行、推理闭合无增量 0ms 拦截、正文抢跑拦截、
// 终态/保底超时拦截、初始等待。
func TestClassifyQualityHoldDecisionTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sig  QualityStreamSignals
		want QualityVerdict
	}{
		{
			name: "initial zero-output waits for thinking delta",
			sig:  QualityStreamSignals{},
			want: QualityWait,
		},
		{
			name: "thinking delta delivers immediately",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 4},
			want: QualityDeliver,
		},
		{
			name: "reasoning ended without thinking withholds immediately",
			sig:  QualityStreamSignals{ReasoningEndedWithoutThinking: true},
			want: QualityWithhold,
		},
		{
			name: "reasoning ended outranks visible output",
			sig:  QualityStreamSignals{ReasoningEndedWithoutThinking: true, HasThinking: false, VisibleTokens: 500},
			want: QualityWithhold,
		},
		{
			name: "thinking outranks reasoning-ended signal",
			sig:  QualityStreamSignals{ReasoningEndedWithoutThinking: true, HasThinking: true},
			want: QualityDeliver,
		},
		{
			name: "any visible output without thinking withholds (body outrun rule)",
			sig:  QualityStreamSignals{VisibleTokens: 1},
			want: QualityWithhold,
		},
		{
			name: "large visible output without thinking withholds",
			sig:  QualityStreamSignals{VisibleTokens: 500},
			want: QualityWithhold,
		},
		{
			name: "terminal with output without thinking withholds",
			sig:  QualityStreamSignals{VisibleTokens: 8, Terminal: true},
			want: QualityWithhold,
		},
		{
			name: "terminal with zero output withholds (empty-stream short-circuit owns the real path)",
			sig:  QualityStreamSignals{Terminal: true},
			want: QualityWithhold,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyQualityHoldShadowed(tc.sig); got != tc.want {
				t.Fatalf("classifyQualityHoldShadowed(%+v) = %s, want %s", tc.sig, got, tc.want)
			}
		})
	}
}
