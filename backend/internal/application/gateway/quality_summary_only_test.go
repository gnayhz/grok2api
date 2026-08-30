package gateway

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// TestSummaryOnlyThinkingConvertsToEvidence：Console 形态的健康流——思考
// 只经 reasoning_summary_text.delta（无 raw reasoning_text），转换器延迟
// 缓冲到 item.done 才冲刷为 thinking_delta。端到端锁：该形态必须经真
// 转换器产出证据并被守卫放行（延迟冲刷路径此前无契约覆盖）。
func TestSummaryOnlyThinkingConvertsToEvidence(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		"data: " + `{"type":"response.reasoning_summary_text.delta","delta":"summarized thought"}`,
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning"}}`,
		"data: " + `{"type":"response.output_text.delta","delta":"answer"}`,
		"data: " + `{"type":"response.completed","response":{"id":"resp_s"}}`,
		"",
	}, "\n\n")
	converted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(stream)), conversation.OperationMessages, conversation.ResponseOptions{AnthropicThinking: true})
	replay, verdict, _, err := peekQualityStream(context.Background(), converted, qualityProtocolAnthropic, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("summary-only thinking verdict = %s, want deliver (deferred summary flush is evidence)", verdict)
	}
}
