package gateway

import (
	"testing"
)

// TestConvertedShapeDifferentialAgreement：round 73 差分锁的转换形态延伸。
// 同一证据以（a）转换后的 chat/messages body 与（b）等价 SSE 事件流两种
// 表达出现时，两条判决路径必须同判。每对夹具由转换器真实输出形状构造。
func TestConvertedShapeDifferentialAgreement(t *testing.T) {
	t.Parallel()
	nl := "\n\n"
	cases := []struct {
		name     string
		protocol string
		body     string
		stream   string
	}{
		{
			name:     "chat healthy thinking",
			protocol: qualityProtocolChat,
			body:     `{"choices":[{"message":{"reasoning_content":"plan it","content":"the answer"}}]}`,
			stream:   "data: " + `{"choices":[{"delta":{"reasoning_content":"plan it"}}]}` + nl + "data: " + `{"choices":[{"delta":{"content":"the answer"}}]}` + nl + "data: [DONE]" + nl,
		},
		{
			name:     "chat degraded outrun",
			protocol: qualityProtocolChat,
			body:     `{"choices":[{"message":{"content":"bare answer without thinking"}}]}`,
			stream:   "data: " + `{"choices":[{"delta":{"content":"bare answer without thinking"}}]}` + nl + "data: [DONE]" + nl,
		},
		{
			name:     "anthropic healthy thinking",
			protocol: qualityProtocolAnthropic,
			body:     `{"content":[{"type":"thinking","thinking":"reason it"},{"type":"text","text":"final"}]}`,
			stream:   "data: " + `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reason it"}}` + nl + "data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"final"}}` + nl + "data: " + `{"type":"message_stop"}` + nl,
		},
		{
			name:     "anthropic redacted-only both take empty path",
			protocol: qualityProtocolAnthropic,
			body:     `{"content":[{"type":"redacted_thinking","data":"sig"}]}`,
			stream:   "data: " + `{"type":"content_block_start","content_block":{"type":"redacted_thinking"}}` + nl + "data: " + `{"type":"content_block_stop"}` + nl + "data: " + `{"type":"message_stop"}` + nl,
		},
		{
			name:     "anthropic degraded outrun",
			protocol: qualityProtocolAnthropic,
			body:     `{"content":[{"type":"text","text":"no thinking at all"}]}`,
			stream:   "data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"no thinking at all"}}` + nl + "data: " + `{"type":"message_stop"}` + nl,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertRawPathAgreement(t, tc.body, tc.stream, tc.protocol)
		})
	}
}
