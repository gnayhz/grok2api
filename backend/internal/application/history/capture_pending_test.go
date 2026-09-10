package history

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestPendingCaptureRequiresAcceptance(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, acceptBeforeClose := range []bool{false, true} {
			t.Run(fmtCase(streaming, acceptBeforeClose), func(t *testing.T) {
				store := memory.NewReasoningReplayStore(10)
				replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
				ctx := context.Background()
				const model = "grok-4.6"
				const session = "guarded"
				old := []byte(`{"output":[{"type":"reasoning","encrypted_content":"` + validEncrypted(91) + `"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}]}`)
				replay.StoreFromCompleted(ctx, model, session, old)
				snapshot := func() []byte {
					items, _, err := store.Get(ctx, model, session, time.Now(), time.Hour)
					if err != nil {
						t.Fatal(err)
					}
					data, _ := json.Marshal(items)
					return data
				}
				before := snapshot()
				payload := `{"output":[{"type":"reasoning","encrypted_content":"` + validEncrypted(92) + `"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"new answer"}]}]}`
				if streaming {
					payload = "data: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\n"
				}
				rejected, _ := replay.CapturePendingBody(io.NopCloser(strings.NewReader(payload)), model, session, streaming, false)
				io.Copy(io.Discard, rejected)
				rejected.Close()
				if !bytes.Equal(snapshot(), before) {
					t.Fatal("withheld completed payload changed existing context")
				}
				accepted, accept := replay.CapturePendingBody(io.NopCloser(strings.NewReader(payload)), model, session, streaming, false)
				io.Copy(io.Discard, accepted)
				if acceptBeforeClose {
					accept()
				}
				accepted.Close()
				if !acceptBeforeClose {
					if !bytes.Equal(snapshot(), before) {
						t.Fatal("Close committed before acceptance")
					}
					accept()
				}
				after := snapshot()
				if bytes.Equal(after, before) {
					t.Fatal("accepted response was never committed")
				}
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(func() { accept(); accepted.Close() })
				}
				wg.Wait()
				if !bytes.Equal(snapshot(), after) {
					t.Fatal("repeated acceptance changed context")
				}
			})
		}
	}
}
func fmtCase(streaming, before bool) string {
	if streaming {
		if before {
			return "stream_accept_before_close"
		}
		return "stream_accept_after_close"
	}
	if before {
		return "json_accept_before_close"
	}
	return "json_accept_after_close"
}

func TestCaptureSSEPartitionsPreserveOnlyReplayEvents(t *testing.T) {
	first := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}\n")
	delta := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", 128<<10) + "\"}\n")
	last := []byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n")
	want := append(append([]byte{}, first...), last...)
	payload := append(append(append([]byte{}, first...), delta...), last...)
	for _, size := range []int{1, 17, 32 << 10, len(payload)} {
		body := &replayCaptureBody{}
		for offset := 0; offset < len(payload); offset += size {
			body.observeSSE(payload[offset:min(offset+size, len(payload))])
		}
		if body.truncated || len(body.pending) != 0 || !bytes.Equal(body.buf.Bytes(), want) {
			t.Fatalf("partition=%d lost or retained unexpected SSE events", size)
		}
	}
}

func TestCaptureOverflowDropsBuffersAndPreservesDeliveredBytes(t *testing.T) {
	// The replay cache may stop capturing, but the healthy client stream must
	// still receive every byte. Keep the wrapper alive to check retained memory.
	payload := []byte(strings.Repeat("x", maxReplayCaptureBytes+1))
	for _, streaming := range []bool{false, true} {
		body := &replayCaptureBody{inner: io.NopCloser(bytes.NewReader(payload)), streaming: streaming}
		body.buf.WriteString("previous retained event")
		got, err := io.ReadAll(body)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("streaming=%v: capture changed client bytes: %v", streaming, err)
		}
		if !body.truncated || body.buf.Cap() != 0 || cap(body.pending) != 0 {
			t.Fatalf("streaming=%v: over-budget capture retained buffers", streaming)
		}
		_ = body.Close()
	}
}
