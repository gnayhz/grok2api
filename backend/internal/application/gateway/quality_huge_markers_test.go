package gateway

import (
	"strings"
	"testing"
)

// TestClassifyHugeQualityLineProtocolMarkers locks the round-16 protocol
// symmetrization of huge-line visible/thinking markers; the coverage
// profile showed these branches walked by no test at the time.
func TestClassifyHugeQualityLineProtocolMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, line            string
		wantThink, wantVisual bool
	}{
		{name: "responses output_text delta", line: strings.Repeat("a", 64) + `{"type":"response.output_text.delta","delta":"word"}`, wantThink: false, wantVisual: true},
		{name: "anthropic text_delta", line: strings.Repeat("a", 64) + `{"type":"text_delta","text":"word"}`, wantThink: false, wantVisual: true},
		{name: "chat delta content", line: strings.Repeat("a", 64) + `{"delta":{"content":"word"}}`, wantThink: false, wantVisual: true},
		{name: "responses reasoning delta", line: strings.Repeat("a", 64) + `{"type":"response.reasoning_text.delta"}`, wantThink: true, wantVisual: false},
		{name: "anthropic thinking delta", line: strings.Repeat("a", 64) + `{"type":"thinking_delta"}`, wantThink: true, wantVisual: false},
		{name: "ciphertext skipped", line: strings.Repeat("a", 64) + `{"encrypted_content":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"}`, wantThink: false, wantVisual: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := &qualityScanState{}
			classifyHugeQualityLine(state, []byte(tc.line))
			if state.hasThinking != tc.wantThink || (state.visibleRunes > 0) != tc.wantVisual {
				t.Fatalf("hasThinking=%t visibleRunes=%d, want think=%t visual=%t", state.hasThinking, state.visibleRunes, tc.wantThink, tc.wantVisual)
			}
		})
	}
}
