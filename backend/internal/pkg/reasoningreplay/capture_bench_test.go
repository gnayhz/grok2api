package reasoningreplay

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func BenchmarkCaptureBodyReasoningStream(b *testing.B) {
	enc := validEncrypted(21)
	var body strings.Builder
	for i := 0; i < 2000; i++ {
		body.WriteString("data: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"hmm\"}\n\n")
	}
	body.WriteString("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"encrypted_content\":\"" + enc + "\"}}\n\n")
	body.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n")
	payload := body.String()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.Run("old_buffer_all", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var buf strings.Builder
			buf.WriteString(payload)
			_, _ = extractCompletedPayloadFromSSE([]byte(buf.String()))
		}
	})
	b.Run("new", func(b *testing.B) {
		replay := New(memory.NewReasoningReplayStore(8), Config{Enabled: true}, slog.Default())
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			wrapped := replay.CaptureBody(io.NopCloser(strings.NewReader(payload)), "grok-4.5", "bench", true, false)
			_, _ = io.Copy(io.Discard, wrapped)
			_ = wrapped.Close()
		}
	})
}
