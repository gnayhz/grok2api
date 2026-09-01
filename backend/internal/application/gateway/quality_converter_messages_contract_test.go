package gateway

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// TestConvertedMessagesStreamContract 锁定 Messages(Anthropic) 路径的转换器
// ↔扫描器证据契约（chat 路径的镜像，见 quality_converter_contract_test.go）：
// 客户端显式请求 thinking 时，转换器以 thinking_delta 转发思考增量，扫描器
// 必须读作证据放行；密文降智流转换后同样零思考，必须扣留。
func TestConvertedMessagesStreamDeliversThinkingEvidence(t *testing.T) {
	t.Parallel()
	upstream := strings.Join([]string{
		"data: " + `{"type":"response.created","response":{"id":"resp_1"}}`,
		"data: " + `{"type":"response.reasoning_text.delta","delta":"think it through"}`,
		"data: " + `{"type":"response.output_text.delta","delta":"final answer"}`,
		"data: " + `{"type":"response.completed","response":{"status":"completed"}}`,
		"",
	}, "\n\n")
	opts := conversation.ResponseOptions{AnthropicThinking: true}
	converted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(upstream)), conversation.OperationMessages, opts)
	replay, verdict, _, err := peekQualityStream(context.Background(), converted, qualityProtocolAnthropic, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("converted healthy messages stream verdict = %s, want deliver", verdict)
	}
}

// TestConvertedMessagesAdaptiveThinkingDelivers: round 39 aligned the guard
// vocabulary with the converter for adaptive thinking; this locks the
// converter side - the same reasoning deltas converted under the
// AnthropicThinking flag (which adaptive requests map to) must carry
// thinking_delta evidence and deliver.
func TestConvertedMessagesAdaptiveThinkingDelivers(t *testing.T) {
	t.Parallel()
	upstream := strings.Join([]string{
		"data: " + `{"type":"response.reasoning_text.delta","delta":"adaptive think"}`,
		"data: " + `{"type":"response.output_text.delta","delta":"answer"}`,
		"",
	}, "\n\n")
	converted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(upstream)), conversation.OperationMessages, conversation.ResponseOptions{AnthropicThinking: true})
	replay, verdict, _, err := peekQualityStream(context.Background(), converted, qualityProtocolAnthropic, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("adaptive thinking stream verdict = %s, want deliver", verdict)
	}
}

func TestConvertedMessagesDegradedStreamWithholds(t *testing.T) {
	t.Parallel()
	ciphertext := strings.Repeat("pad ", 2048)
	upstream := strings.Join([]string{
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"XXCIPHERTEXTXX"}}`,
		"data: " + `{"type":"response.output_text.delta","delta":"bare answer without thinking"}`,
		"",
	}, "\n\n")
	upstream = strings.ReplaceAll(upstream, "XXCIPHERTEXTXX", ciphertext)
	opts := conversation.ResponseOptions{AnthropicThinking: true}
	converted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(upstream)), conversation.OperationMessages, opts)
	replay, verdict, _, err := peekQualityStream(context.Background(), converted, qualityProtocolAnthropic, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("converted degraded messages stream verdict = %s, want withhold", verdict)
	}
}

// item.added 会开 thinking 块，item.done 密文会发 signature_delta。
// 签名不是 thinking_delta：转换后守卫仍扣留，密文不得当思考放行。
func TestMessagesConvertCiphertextIsNotThinkingDelta(t *testing.T) {
	t.Parallel()
	upstream := strings.Join([]string{
		"data: " + `{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"EqoBCkgIYj"}}`,
		"data: " + `{"type":"response.output_text.delta","delta":"bare answer"}`,
		"data: " + `{"type":"response.completed","response":{"status":"completed"}}`,
		"",
	}, "\n\n")
	converted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(upstream)), conversation.OperationMessages, conversation.ResponseOptions{AnthropicThinking: true})
	raw, err := io.ReadAll(converted)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, `"type":"thinking_delta"`) {
		t.Fatal("ciphertext-only convert must not emit thinking_delta")
	}
	if !strings.Contains(text, `"type":"signature_delta"`) {
		t.Fatal("ciphertext-only convert should emit signature_delta after thinking block start")
	}
	replay, verdict, _, peekErr := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(text)), qualityProtocolAnthropic, QualityRetryRuntime{})
	if replay != nil {
		_ = replay.Close()
	}
	if peekErr != nil || verdict != QualityWithhold {
		t.Fatalf("converted ciphertext-only peek = %s err=%v, want withhold (signature is not thinking)", verdict, peekErr)
	}
}
