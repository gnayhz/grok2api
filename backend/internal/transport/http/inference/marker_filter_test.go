package inference

import (
	"bytes"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// 内部思考证据注释必须在转发前剥除（含跨 chunk 边界分裂的形态）。
func TestInternalSSEMarkerFilterStripsEvidenceComment(t *testing.T) {
	t.Parallel()
	marker := []byte(conversation.ThinkingEvidenceComment + "\n\n")
	filter := internalSSEMarkerFilter{enabled: true}
	var out bytes.Buffer
	out.Write(filter.Filter([]byte("event: message_start\n\ndata: {\"type\":\"message_start\"}\n\n"), false))
	// 标记分裂在两个 chunk 里。
	half := len(marker) / 2
	out.Write(filter.Filter(marker[:half], false))
	out.Write(filter.Filter(append(marker[half:], []byte("event: content_block_start\n\n")...), false))
	out.Write(filter.Filter([]byte("data: done\n\n"), false))
	tail := filter.Filter(nil, true)
	out.Write(tail)
	if bytes.Contains(out.Bytes(), marker) {
		t.Fatalf("marker leaked to client: %q", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("message_start")) || !bytes.Contains(out.Bytes(), []byte("content_block_start")) {
		t.Fatalf("normal events must pass through: %q", out.String())
	}
}

// 客户端可见 thinking_delta 不是内部注释，不得被剥掉。
func TestInternalSSEMarkerFilterKeepsThinkingDelta(t *testing.T) {
	t.Parallel()
	filter := internalSSEMarkerFilter{enabled: true}
	in := []byte("event: content_block_delta\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n")
	got := append([]byte(nil), filter.Filter(in, false)...)
	got = append(got, filter.Filter(nil, true)...)
	if !bytes.Contains(got, []byte(`"thinking":"plan"`)) {
		t.Fatalf("thinking_delta must not be stripped: %q", got)
	}
	if bytes.Contains(got, []byte(conversation.ThinkingEvidenceComment)) {
		t.Fatal("filter must not inject the internal marker")
	}
}

// 关闭（非 Anthropic 协议）时透传。
func TestInternalSSEMarkerFilterDisabledPassthrough(t *testing.T) {
	t.Parallel()
	filter := internalSSEMarkerFilter{}
	in := []byte("data: {\"ok\":true}\n\n")
	if got := filter.Filter(in, false); !bytes.Equal(got, in) {
		t.Fatalf("disabled filter must passthrough, got %q", got)
	}
}
