package gateway

import (
	"testing"
)

// TestStreamAndBodyJudgmentsAgree：同一证据集合的两种表达（Responses 完整
// body vs 等价 SSE 事件流）必须得到同一判决——这是流式/非流式两条判决
// 路径的差分一致性锁。任何只修一侧的形状改动都会在此爆掉。
func TestStreamAndBodyJudgmentsAgree(t *testing.T) {
	t.Parallel()
	nl := "\n\n"
	cases := []struct {
		name   string
		body   string
		stream string
	}{
		{name: "healthy thinking",
			body:   `{"output":[{"type":"reasoning","summary":[{"text":"think"}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`,
			stream: "data: " + `{"type":"response.reasoning_text.delta","delta":"think"}` + nl + "data: " + `{"type":"response.output_text.delta","delta":"answer"}` + nl},
		{name: "degraded ciphertext only",
			body:   `{"output":[{"type":"reasoning"}]}`,
			stream: "data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"xx"}}` + nl},
		{name: "body outrun",
			body:   `{"output":[{"type":"message","content":[{"type":"output_text","text":"bare answer"}]}]}`,
			stream: "data: " + `{"type":"response.output_text.delta","delta":"bare answer"}` + nl},
		{name: "aggregate only message",
			body:   `{"output":[{"type":"message","content":[{"type":"output_text","text":"full text"}]}]}`,
			stream: "data: " + `{"type":"response.output_item.done","item":{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"full text"}]}}` + nl},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertRawPathAgreement(t, tc.body, tc.stream, qualityProtocolResponses)
		})
	}
}
