package gateway

import (
	"strings"
	"testing"
)

// TestResponsesMarkerAloneWithholds / TestAnthropicMarkerAloneWithholds：
// responses 的 reasoning item 头与 anthropic 的 thinking 块头单独出现（无文本增量）
// 都是降智形态，必须扣留——帧头本身不构成思考证据。
func TestResponsesMarkerAloneWithholds(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&state, []byte(strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"answer without any reasoning text"}`,
		`data: {"type":"response.completed","response":{"usage":{"output_tokens":64,"output_tokens_details":{"reasoning_tokens":60}}}}`,
		"",
	}, "\n")))
	if classifyQualityHold(state.signals()) != QualityWithhold {
		t.Fatalf("responses item header alone must withhold: %#v", state.signals())
	}
}

func TestAnthropicMarkerAloneWithholds(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolAnthropic}
	observeQualityChunk(&state, []byte(strings.Join([]string{
		`data: {"type":"content_block_start","content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"answer without thinking deltas"}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")))
	if classifyQualityHold(state.signals()) != QualityWithhold {
		t.Fatalf("anthropic thinking block header alone must withhold: %#v", state.signals())
	}
}
