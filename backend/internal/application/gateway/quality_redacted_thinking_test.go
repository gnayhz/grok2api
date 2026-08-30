package gateway

import (
	"strings"
	"testing"
)

// TestAnthropicRedactedThinkingIsNotEvidenceNorSemantic：redacted_thinking 是
// 加密思考块——既不是思考证据（与 encrypted_content/signature_delta 同理），
// 也不得算作纯语义输出而直接交付。分类器层（规则 4）扣留是空流短路后的
// 纵深防御；peek 层的实际收口是空流路径（见 TestStreamRedactedOnlyTakesEmptyStreamPath，
// redacted 也可能是健康账号的隐私脱敏，走 15m 空闲而非定罪）。
func TestAnthropicRedactedThinkingIsNotEvidenceNorSemantic(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolAnthropic}
	observeQualityChunk(&state, []byte(strings.Join([]string{
		"data: " + `{"type":"content_block_start","content_block":{"type":"redacted_thinking"}}`,
		"data: " + `{"type":"content_block_stop"}`,
		"data: " + `{"type":"message_stop"}`,
		"",
	}, "\n")))
	sig := state.signals()
	if sig.HasThinking {
		t.Fatal("redacted thinking must not count as thinking evidence")
	}
	if state.semanticOutput {
		t.Fatal("redacted thinking must not count as semantic output (delivery laundering)")
	}
	if v := classifyQualityHold(sig); v != QualityWithhold {
		t.Fatalf("terminal redacted-only stream verdict = %s, want withhold (rule 4)", v)
	}
}
