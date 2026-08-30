package gateway

import (
	"io"
	"strings"
	"testing"
)

// TestPeekQualityBodyClassifiesConvertedClientShapes：非流式 chat/messages
// 请求的 body 在 adapter ForwardResponse 内已被转换成客户端形态——
// body 判决必须认识这些形状（此前 Output==nil 一律 fail-open，非流式
// chat/messages 整体绕过守卫）。锁三形态：健康 chat（带 reasoning_content）
// 放行、降智 chat（零思考有正文）扣留、messages 形态同理。
func TestPeekQualityBodyClassifiesConvertedClientShapes(t *testing.T) {
	t.Parallel()
	healthyChat := `{"id":"chatcmpl-1","choices":[{"message":{"reasoning_content":"plan then answer","content":"the answer"}}]}`
	degradedChat := `{"id":"chatcmpl-2","choices":[{"message":{"content":"a bare answer with no thinking"}}]}`
	healthyMessages := `{"id":"msg-1","content":[{"type":"thinking","thinking":"reason it out"},{"type":"text","text":"final"}]}`
	degradedMessages := `{"id":"msg-2","content":[{"type":"text","text":"no thinking at all"}]}`
	toolOnlyChat := `{"choices":[{"message":{"tool_calls":[{"id":"call-1"}]}}]}`
	alien := `{"result":"ok"}`
	cases := []struct {
		name          string
		body          string
		want          QualityVerdict
		wantUsage     int64
		wantUsageSeen bool
		wantErr       bool
	}{
		{name: "healthy chat delivers", body: healthyChat, want: QualityDeliver},
		{name: "chat usage flows through", body: `{"id":"chatcmpl-u","model":"grok-4.6","usage":{"prompt_tokens":9,"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":12}},"choices":[{"message":{"reasoning_content":"plan","content":"answer"}}]}`, want: QualityDeliver, wantUsage: 40, wantUsageSeen: true},
		{name: "degraded chat withholds", body: degradedChat, want: QualityWithhold},
		{name: "healthy messages delivers", body: healthyMessages, want: QualityDeliver},
		{name: "degraded messages withholds", body: degradedMessages, want: QualityWithhold},
		// redacted-only 形态按空流收口（与流式路径一致）：redacted 是密文而非证据，
		// 但也可能是健康账号的隐私脱敏——不定罪，走 15m 空闲路径（round 34 同理）。
		{name: "redacted-only messages takes empty-stream path", body: `{"content":[{"type":"redacted_thinking","data":"sig"}]}`, want: QualityWait, wantErr: true},
		{name: "tool-only chat delivers", body: toolOnlyChat, want: QualityDeliver},
		{name: "alien shape still fail-opens", body: alien, want: QualityDeliver},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			replay, verdict, usage, err := peekQualityBody(nopCloserString(tc.body), QualityRetryRuntime{})
			if tc.wantErr {
				if err == nil || verdict != tc.want {
					t.Fatalf("verdict=%s err=%v, want %s + empty-stream error", verdict, err, tc.want)
				}
			} else if err != nil {
				t.Fatalf("peek err = %v", err)
			}
			if replay != nil {
				defer replay.Close()
			}
			if verdict != tc.want {
				t.Fatalf("verdict = %s, want %s", verdict, tc.want)
			}
			if tc.wantUsageSeen && (!usage.Reported || usage.OutputTokens != tc.wantUsage) {
				t.Fatalf("usage = %#v, want reported output=%d", usage, tc.wantUsage)
			}
		})
	}
}

func nopCloserString(value string) io.ReadCloser { return io.NopCloser(strings.NewReader(value)) }
