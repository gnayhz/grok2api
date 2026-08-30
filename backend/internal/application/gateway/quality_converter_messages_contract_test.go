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
