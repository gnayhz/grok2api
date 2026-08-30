package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCoverageGapTriple: three zero-coverage branches from the profile -
// aggregate anthropic text at block_stop, non-stream anthropic usage,
// and function_call aggregate items as semantic output.
func TestCoverageGapTriple(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		"data: " + `{"type":"content_block_start","content_block":{"type":"text"}}`,
		"data: " + `{"type":"content_block_stop","content_block":{"type":"text","text":"full answer at block end"}}`,
		"data: " + `{"type":"message_stop"}`,
		"",
	}, "\n\n")
	streamState := qualityScanState{protocol: qualityProtocolAnthropic}
	observeQualityChunk(&streamState, []byte(stream))
	if streamState.visibleRunes == 0 || !streamState.semanticOutput {
		t.Fatalf("aggregate block_stop: visible=%d semantic=%t", streamState.visibleRunes, streamState.semanticOutput)
	}
	anthropicBody := `{"id":"msg-u","model":"grok-4.6","usage":{"input_tokens":7,"output_tokens":21,"output_tokens_details":{"thinking_tokens":9}},"content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"done"}]}`
	replay, verdict, usage, err := peekQualityBody(nopCloserString(anthropicBody), QualityRetryRuntime{})
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("anthropic body verdict=%s err=%v", verdict, err)
	}
	if replay != nil {
		_ = replay.Close()
	}
	if !usage.Reported || usage.OutputTokens != 21 || usage.ReasoningTokens != 9 {
		t.Fatalf("anthropic usage = %#v", usage)
	}
	var parsed qualityResponseBody
	if err := json.Unmarshal([]byte(`{"output":[{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{}"}]}`), &parsed); err != nil {
		t.Fatal(err)
	}
	callState := qualityScanState{protocol: qualityProtocolResponses}
	for i := range parsed.Output {
		noteResponsesAggregateOutput(&callState, qualityReasoningItem{Type: parsed.Output[i].Type})
	}
	if !callState.semanticOutput {
		t.Fatal("function_call aggregate item must count as semantic output")
	}
}
