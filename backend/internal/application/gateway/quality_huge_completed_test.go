package gateway

import (
	"strings"
	"testing"
)

func TestHugeCompletedStillSetsTerminalAndUsage(t *testing.T) {
	t.Parallel()
	cipher := strings.Repeat("A", 64<<10+128)
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi there world\"}\n"
	body += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"},{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}],\"usage\":{\"output_tokens\":95,\"output_tokens_details\":{\"reasoning_tokens\":40}}}}\n"
	state := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&state, []byte(body))
	if !state.terminal {
		t.Fatal("huge completed must set terminal")
	}
	if !state.usage.Reported || state.usage.OutputTokens != 95 || state.usage.ReasoningTokens != 40 {
		t.Fatalf("usage = %+v", state.usage)
	}
	if state.responseID != "resp_1" {
		t.Fatalf("id = %q", state.responseID)
	}
	if state.aggregateRunes == 0 {
		t.Fatal("aggregate text from completed tail was dropped")
	}
}
