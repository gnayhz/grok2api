package inference

import (
	"bytes"
	"strings"
	"testing"
)

// 复现线上 upstream_stream_incomplete 回归（2026-08-27 线上 5/5 失败）:
// copyStream 对 streamProtocolResponses 先 rewriteResponsesStreamChunk 再 Inspect。
// completed 帧被 sanitize 后经 map[string]any 重排键序（字母序 response < type），
// "type" 落到多 KB response 对象之后；sseEventType 只看头 4096 字节的根层键，
// 在截断的 response 值处提前返回空串 → terminalSuccess 永不置位。
func TestRewrittenCompletedFrameStillTerminal(t *testing.T) {
	// 与线上 grok-4.6 相近：completed 带 reasoning summary + message 全文。
	text := strings.Repeat("段", 2600) // ~7.8KB UTF-8, pushes rewritten frame past 4096
	upstream := []byte("data: " + strings.ReplaceAll(
		`{"type":"response.completed","sequence_number":41,"response":{"id":"resp_live_1","created_at":1787863000,"model":"grok-4.6","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"__TEXT__"}],"content":[]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":420,"output_tokens":1033,"output_tokens_details":{"reasoning_tokens":1027},"total_tokens":1453}}}`,
		"__TEXT__", text) + "\n")

	// 生产管线：先 compat 重写（补 object/output/model/annotations → 重排键序）
	state := &responsesCompatState{}
	rewritten := rewriteResponsesDataLine(upstream, state)
	if len(rewritten) <= 4096 {
		t.Fatalf("rewritten frame should exceed 4096 bytes to mirror live wire, got %d", len(rewritten))
	}
	typeOffset := bytes.Index(rewritten, []byte(`"type":"response.completed"`))
	if typeOffset > 4096 {
		t.Logf("note: type sits at offset %d (after sorted response object)", typeOffset)
	}

	inspector := &responseInspector{protocol: streamProtocolResponses}
	inspector.Inspect(rewritten)
	inspector.Finish()
	if err := inspector.TerminalError(); err != nil {
		t.Fatalf("completed stream misclassified: %v", err)
	}
	meta := inspector.Metadata()
	if meta.Usage.OutputTokens != 1033 || meta.Usage.ReasoningTokens != 1027 {
		t.Fatalf("usage out=%d reasoning=%d (want 1033/1027)", meta.Usage.OutputTokens, meta.Usage.ReasoningTokens)
	}
	if meta.ResponseID != "resp_live_1" {
		t.Fatalf("ResponseID = %q", meta.ResponseID)
	}
}

// 锁定 responses 协议在「重写后键序」下的行为，防止再次只在原始键序上做等价测试。
func TestSSEEventTypeOnSortedKeyFrame(t *testing.T) {
	frame := []byte(`{"response":{"id":"resp_9","usage":{"output_tokens":7}},"sequence_number":9,"type":"response.completed"}`)
	if got := sseEventType(frame); got != "response.completed" {
		t.Fatalf("sseEventType = %q, want response.completed", got)
	}
}
