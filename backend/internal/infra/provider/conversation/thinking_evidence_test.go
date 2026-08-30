package conversation

import (
	"bytes"
	"testing"
)

// 未请求 thinking 的 Messages 流式请求：上游可见思考文本必须以内部注释
// 保留为守卫证据（修复——原实现直接丢弃，守卫失去唯一证据
// 通道，15 条零思考降智交付经豁解放行，见 gateway REASONING0_LEDGER §C2）。
func TestThinkingEvidenceCommentEmittedWithoutAnthropicThinking(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	converter := newStreamConverter(&out, OperationMessages, ResponseOptions{})
	if err := converter.handle("response.reasoning_summary_text.delta", []byte(`{"type":"response.reasoning_summary_text.delta","delta":"对比体感温度"}`)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(ThinkingEvidenceComment)) {
		t.Fatalf("visible thinking on no-think messages must emit evidence comment, got: %q", out.String())
	}
	if bytes.Contains(out.Bytes(), []byte("thinking_delta")) {
		t.Fatal("evidence path must not leak thinking_delta when client did not request thinking")
	}
}

// raw reasoning_text.delta 同样触发证据注释（Console 同思路上报两通道）。
func TestThinkingEvidenceCommentOnRawTextDelta(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	converter := newStreamConverter(&out, OperationMessages, ResponseOptions{})
	if err := converter.handle("response.reasoning_text.delta", []byte(`{"type":"response.reasoning_text.delta","delta":"hmm"}`)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(ThinkingEvidenceComment)) {
		t.Fatal("raw reasoning delta must emit evidence comment")
	}
}

// 空白增量（summary 尾部补发的纯换行等）不是思考证据，不得写证据
// 注释——否则守卫扫描端会把零思考降智流误判为有证据而整包放行。
func TestBlankDeltaDoesNotEmitEvidenceComment(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	converter := newStreamConverter(&out, OperationMessages, ResponseOptions{})
	if err := converter.handle("response.reasoning_summary_text.delta", []byte(`{"type":"response.reasoning_summary_text.delta","delta":"
"}`)); err != nil {
		t.Fatal(err)
	}
	if err := converter.handle("response.reasoning_text.delta", []byte(`{"type":"response.reasoning_text.delta","delta":" 	 "}`)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte(ThinkingEvidenceComment)) {
		t.Fatalf("blank delta must not emit evidence comment, got: %q", out.String())
	}
	if err := converter.handle("response.reasoning_text.delta", []byte(`{"type":"response.reasoning_text.delta","delta":"hmm"}`)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(ThinkingEvidenceComment)) {
		t.Fatal("visible delta must still emit evidence comment")
	}
}

// thinking 已启用 / chat 协议：思考增量本身可见，不得出现证据注释。
func TestNoEvidenceCommentWhenThinkingEnabledOrChat(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	enabled := newStreamConverter(&out, OperationMessages, ResponseOptions{AnthropicThinking: true})
	if err := enabled.handle("response.reasoning_text.delta", []byte(`{"type":"response.reasoning_text.delta","delta":"hmm"}`)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte(ThinkingEvidenceComment)) {
		t.Fatal("thinking-enabled messages must forward thinking_delta, not the comment")
	}
	if !bytes.Contains(out.Bytes(), []byte("thinking_delta")) {
		t.Fatal("thinking-enabled messages must forward thinking_delta")
	}
	var chatOut bytes.Buffer
	chat := newStreamConverter(&chatOut, OperationChat, ResponseOptions{})
	if err := chat.handle("response.reasoning_text.delta", []byte(`{"type":"response.reasoning_text.delta","delta":"hmm"}`)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(chatOut.Bytes(), []byte(ThinkingEvidenceComment)) {
		t.Fatal("chat reasoning is visible via reasoning_content; comment must not appear")
	}
	if !bytes.Contains(chatOut.Bytes(), []byte("reasoning_content")) {
		t.Fatal("chat must forward reasoning_content")
	}
}
