package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestAnthropicEmptyStreamIsNotEmpty200：Anthropic 协议空流（仅 message_stop 或直接 EOF，零内容零 thinking 增量）
// 必须按空流处理（D 审查缺口）——不得 fail-open 成可投递的 200。
func TestAnthropicEmptyStreamIsNotEmpty200(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: 200 * time.Millisecond}
	cases := map[string]string{
		"message-stop-only": "data: {\"type\":\"message_stop\"}\n\n",
		"eof-only":          "",
		// usage-only（D 缺口）：只带 usage 声明（thinking tokens）零内容零
		// thinking 增量——与 chat/responses 同语义，必须判空流。
		"usage-only": "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":41,\"output_tokens_details\":{\"thinking_tokens\":33}}}\n\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	for name, body := range cases {
		replay, verdict, _, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolAnthropic, cfg)
		if err == nil || verdict != QualityWait {
			t.Fatalf("%s: anthropic empty stream must surface empty-stream error, verdict=%s err=%v", name, verdict, err)
		}
		_ = replay.Close()
	}
}
