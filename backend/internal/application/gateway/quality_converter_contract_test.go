package gateway

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// TestConvertedChatStreamDeliversThinkingEvidence 锁定跨层契约：质量扫描器
// 解析的是转换后的客户端侧流（adapter 在 ForwardResponse 内完成协议转换，
// 网关 peek 随后判决）。健康的上游 Responses 流经 chat 转换后必须携带
// reasoning_content 增量，被扫描器读作思考证据并放行——两层的形状契约
// 此前仅靠约定一致，这里把它变成显式回归保证。
func TestConvertedChatStreamDeliversThinkingEvidence(t *testing.T) {
	t.Parallel()
	upstream := strings.Join([]string{
		"data: " + `{"type":"response.reasoning_text.delta","delta":"think step by step"}`,
		"data: " + `{"type":"response.output_text.delta","delta":"the answer is 2"}`,
		"data: " + `{"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":24}}}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	converted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(upstream)), conversation.OperationChat, conversation.ResponseOptions{})
	replay, verdict, _, err := peekQualityStream(context.Background(), converted, qualityProtocolChat, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("converted healthy chat stream verdict = %s, want deliver", verdict)
	}
}

// TestConvertedChatDegradedStreamWithholds：转换器不得发明证据——上游只有
// 密文 reasoning item 与正文（零思考增量）时，转换后的 chat 流同样零思考，
// 必须被扣留。锁“转换不得把降智流洗白”的方向。
func TestConvertedChatDegradedStreamWithholds(t *testing.T) {
	t.Parallel()
	ciphertext := strings.Repeat("pad ", 2048)
	upstream := strings.Join([]string{
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"XXCIPHERTEXTXX"}}`,
		"data: " + `{"type":"response.output_text.delta","delta":"a bare answer with no thinking"}`,
		"",
	}, "\n\n")
	upstream = strings.ReplaceAll(upstream, "XXCIPHERTEXTXX", ciphertext)
	converted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(upstream)), conversation.OperationChat, conversation.ResponseOptions{})
	replay, verdict, _, err := peekQualityStream(context.Background(), converted, qualityProtocolChat, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("converted degraded chat stream verdict = %s, want withhold", verdict)
	}
}
