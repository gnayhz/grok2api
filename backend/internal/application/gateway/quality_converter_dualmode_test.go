package gateway

import (
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// --- quality_converter_dualmode_test.go ---
// TestConverterDualModeEvidenceEquivalence：转换器双模式差分——同一逻辑
// 上游（健康：思考+正文）分别以 JSON（非流式转换）与 SSE 事件（流式转换）
// 表达，经真转换器到真 peek 的判决必须一致。此前各模式分别有契约，但
// 转换器自身的双模式等价性没有锁——某侧的思考发射回归只会在单模式
// 测试里露头，跨模式漂移则无防护。
func TestConverterDualModeEvidenceEquivalence(t *testing.T) {
	t.Parallel()
	nl := "\n\n"
	streamUpstream := strings.Join([]string{
		"data: " + `{"type":"response.reasoning_text.delta","delta":"plan the approach"}`,
		"data: " + `{"type":"response.output_text.delta","delta":"the answer"}`,
		"data: " + `{"type":"response.completed","response":{"id":"resp_d","usage":{"output_tokens":12}}}`,
		"",
	}, nl)
	jsonUpstream := `{"id":"resp_d","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"plan the approach"}]},{"type":"message","content":[{"type":"output_text","text":"the answer"}]}],"usage":{"output_tokens":12}}`

	assertDualModeAgreement(t, jsonUpstream, streamUpstream, conversation.OperationChat, qualityProtocolChat, qualityProtocolChat, conversation.ResponseOptions{}, QualityDeliver)
}

// --- quality_dualmode_degraded_test.go ---
// TestConverterDualModeDegradedEquivalence: the degraded side of the
// dual-mode differential - the same ciphertext degrade upstream must
// withhold through both converter modes (mirror of the round-73 body
// rule-2 symmetry fix).
func TestConverterDualModeDegradedEquivalence(t *testing.T) {
	t.Parallel()
	nl := "\n\n"
	ciphertext := strings.Repeat("pad ", 512)
	streamUpstream := strings.Join([]string{
		"data: " + `{"type":"response.created","response":{"id":"resp_x"}}`,
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"` + ciphertext + `"}}`,
		"data: " + `{"type":"response.output_text.delta","delta":"bare answer"}`,
		"",
	}, nl)
	jsonUpstream := `{"id":"resp_x","output":[{"type":"reasoning","encrypted_content":"` + ciphertext + `"},{"type":"message","content":[{"type":"output_text","text":"bare answer"}]}]}`
	assertDualModeAgreement(t, jsonUpstream, streamUpstream, conversation.OperationChat, qualityProtocolChat, qualityProtocolChat, conversation.ResponseOptions{}, QualityWithhold)
}

// --- quality_dualmode_messages_test.go ---
// TestConverterDualModeMessagesEquivalence: the outermost differential
// ring completed for the Messages protocol - the same healthy upstream
// through both converter modes (JSON content blocks vs content_block
// stream events) must deliver identically with thinking requested.
func TestConverterDualModeMessagesEquivalence(t *testing.T) {
	t.Parallel()
	nl := "\n\n"
	streamUpstream := strings.Join([]string{
		"data: " + `{"type":"response.reasoning_text.delta","delta":"plan"}`,
		"data: " + `{"type":"response.output_text.delta","delta":"answer"}`,
		"data: " + `{"type":"response.completed","response":{"id":"resp_m"}}`,
		"",
	}, nl)
	jsonUpstream := `{"id":"resp_m","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`
	assertDualModeAgreement(t, jsonUpstream, streamUpstream, conversation.OperationMessages, qualityProtocolAnthropic, qualityProtocolAnthropic, conversation.ResponseOptions{AnthropicThinking: true}, QualityDeliver)
}

// --- quality_dualmode_messages_deg_test.go ---
// TestConverterDualModeMessagesDegraded: the degraded messages pair -
// ciphertext reasoning + bare answer must withhold through both
// converter modes (mirror of the chat pair, round 102).
func TestConverterDualModeMessagesDegraded(t *testing.T) {
	t.Parallel()
	nl := "\n\n"
	ciphertext := strings.Repeat("pad ", 512)
	streamUpstream := strings.Join([]string{
		"data: " + `{"type":"response.created","response":{"id":"resp_md"}}`,
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"` + ciphertext + `"}}`,
		"data: " + `{"type":"response.output_text.delta","delta":"bare"}`,
		"",
	}, nl)
	jsonUpstream := `{"id":"resp_md","output":[{"type":"reasoning","encrypted_content":"` + ciphertext + `"},{"type":"message","content":[{"type":"output_text","text":"bare"}]}]}`
	assertDualModeAgreement(t, jsonUpstream, streamUpstream, conversation.OperationMessages, qualityProtocolAnthropic, qualityProtocolAnthropic, conversation.ResponseOptions{AnthropicThinking: true}, QualityWithhold)
}
