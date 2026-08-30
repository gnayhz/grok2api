package gateway

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// ThinkingEvidenceComment（转换器在未请求 thinking 的 Messages 流上看到
// 可见思考文本时写入的内部注释）计为思考证据；普通 SSE 注释仍不计。
func TestScannerCountsThinkingEvidenceComment(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolAnthropic}
	observeQualityChunk(&state, []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	observeQualityChunk(&state, []byte(conversation.ThinkingEvidenceComment+"\n\n"))
	sig := state.signals()
	if !sig.HasThinking {
		t.Fatalf("evidence comment must count as thinking evidence: %#v", sig)
	}
	if v := classifyQualityHold(sig); v != QualityDeliver {
		t.Fatalf("evidence comment must deliver: %s", v)
	}
}

// 对照：任意其它 SSE 注释（keepalive）不构成证据。
func TestScannerIgnoresGenericComments(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolAnthropic}
	observeQualityChunk(&state, []byte(": keepalive\n\n"))
	observeQualityChunk(&state, []byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"答案\"}}\n\n"))
	observeQualityChunk(&state, []byte("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"))
	sig := state.signals()
	if sig.HasThinking {
		t.Fatalf("generic comment must not count as thinking evidence: %#v", sig)
	}
	if v := classifyQualityHold(sig); v != QualityWithhold {
		t.Fatalf("no-think visible text must withhold: %s", v)
	}
}
