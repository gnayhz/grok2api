package inference

import (
	"strings"
	"testing"
)

// 大帧路径与热路径共用 streamTerminalOutcome(单一事实源)。回归背景:
// image_edit.completed 携带整图 base64,必然超过 maxParsedSSEJSONBytes,
// 曾因大帧路径的平行事件表漏掉 image_edit.* 而被误判为
// upstream_stream_incomplete。
func TestHugeImageEditTerminalFrames(t *testing.T) {
	payload := strings.Repeat("A", maxParsedSSEJSONBytes+1024)
	completed := "data: {\"type\":\"image_edit.completed\",\"b64_json\":\"" + payload + "\"}\n\n"
	failed := "data: {\"type\":\"image_edit.failed\",\"error\":\"boom\"}\n\n"

	inspector := &responseInspector{protocol: streamProtocolImage}
	inspector.Inspect([]byte(completed))
	if err := inspector.TerminalError(); err != nil {
		t.Fatalf("huge image_edit.completed must complete the stream, got %v", err)
	}

	inspector = &responseInspector{protocol: streamProtocolImage}
	inspector.Inspect([]byte(failed))
	if err := inspector.TerminalError(); err != errUpstreamStreamFailed {
		t.Fatalf("huge image_edit.failed must fail the stream, got %v", err)
	}
}

// TestStreamTerminalOutcomeTable 锁定每协议的终止事件集合,防止新增事件
// 时只改其中一条路径。
func TestStreamTerminalOutcomeTable(t *testing.T) {
	cases := []struct {
		protocol        streamProtocol
		typ             string
		terminal, succs bool
	}{
		{streamProtocolResponses, "response.completed", true, true},
		{streamProtocolResponses, "response.failed", true, false},
		{streamProtocolResponses, "response.incomplete", true, false},
		{streamProtocolResponses, "response.error", true, false},
		{streamProtocolResponses, "error", true, false},
		{streamProtocolResponses, "response.output_text.delta", false, false},
		{streamProtocolChat, "error", true, false},
		{streamProtocolChat, "message", false, false},
		{streamProtocolAnthropic, "message_stop", true, true},
		{streamProtocolAnthropic, "error", true, false},
		{streamProtocolAnthropic, "content_block_delta", false, false},
		{streamProtocolImage, "image_generation.completed", true, true},
		{streamProtocolImage, "image_edit.completed", true, true},
		{streamProtocolImage, "image_generation.failed", true, false},
		{streamProtocolImage, "image_edit.failed", true, false},
		{streamProtocolImage, "error", true, false},
		{streamProtocolImage, "image_generation.partial_image", false, false},
	}
	for _, tc := range cases {
		terminal, success := streamTerminalOutcome(tc.protocol, tc.typ)
		if terminal != tc.terminal || success != tc.succs {
			t.Errorf("streamTerminalOutcome(%d, %q) = (%v, %v), want (%v, %v)",
				tc.protocol, tc.typ, terminal, success, tc.terminal, tc.succs)
		}
	}
}

func TestCompletionSuccessEventKinds(t *testing.T) {
	for _, kind := range []string{"response.completed", "response.done", "message_stop", "image_generation.completed", "image_edit.completed"} {
		if !completionSuccessEventKind(kind) || !completionSuccessEventType(kind) {
			t.Errorf("kind %q must be a completion success event", kind)
		}
	}
	for _, kind := range []string{"response.failed", "image_edit.failed", "", "message_start"} {
		if completionSuccessEventKind(kind) || completionSuccessEventType(kind) {
			t.Errorf("kind %q must not be a completion success event", kind)
		}
	}
}
