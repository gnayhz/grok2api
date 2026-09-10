package history

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

func TestCanonicalCaptureSharesFramingAndReleasesEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"accepted", "discarded", "interrupted", "exhausted"} {
		t.Run(outcome, func(t *testing.T) {
			pool := responsebuffer.NewPool(1 << 20)
			budget := pool.Request(1 << 20)
			store := memory.NewReasoningReplayStore(10)
			replay := New(store, Config{Enabled: true, TTL: time.Hour}, nil)
			raw := "data: {\"type\":\"response.completed\",\ndata: \"response\":{\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"" + validEncrypted(92) + "\"}]}}\n\n"
			var source io.ReadCloser = io.NopCloser(strings.NewReader(raw))
			if outcome == "interrupted" {
				source = io.NopCloser(io.MultiReader(strings.NewReader(raw), captureFailure{}))
			}
			stream := responseflow.New(source, budget)
			body, accept := replay.CapturePendingBody(stream, "grok-4.6", "session", true, false)
			var reservation *responsebuffer.Lease
			if outcome == "exhausted" {
				reservation, _ = budget.Reserve(1 << 20)
			}
			// Event consumers bypass byte reads; capture must still observe the
			// physical frame and handle multiline data exactly once.
			_ = stream.Hold(4<<20, func(*responseflow.Event) (bool, error) { return true, nil })
			_ = stream.Consume(func(*responseflow.Event) error { return nil })
			_ = body.Close()
			if outcome == "discarded" {
				body.(interface{ DiscardOutput() }).DiscardOutput()
			} else {
				accept()
			}
			reservation.Release()
			items, _, err := store.Get(context.Background(), "grok-4.6", "session", time.Now(), time.Hour)
			data, _ := json.Marshal(items)
			if err != nil || (len(items) > 0) != (outcome == "accepted") {
				t.Fatalf("outcome=%s cache=%s err=%v", outcome, data, err)
			}
			if pool.Snapshot().Used != 0 {
				t.Fatalf("outcome=%s retained=%d", outcome, pool.Snapshot().Used)
			}
		})
	}
}

type captureFailure struct{}

func (captureFailure) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
