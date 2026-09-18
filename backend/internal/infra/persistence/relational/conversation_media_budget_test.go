package relational

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/google/uuid"
)

func TestConversationReplayLargeMediaAcrossColdInstances(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, journal := identityJournalFixture(t, dialect)
			key := "synthetic-media/" + uuid.NewString()
			// Synthetic inline media: 43 images, 32 MiB of JSON input, one message.
			// No network request or real image is needed to exercise durable history.
			image := bytes.Repeat([]byte("A"), (32<<20)/43)
			content := make([]any, 43)
			for i := range content {
				content[i] = map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + string(image)}
			}
			visible := []any{map[string]any{"role": "user", "content": content}}
			pool := responsebuffer.NewPool(responsebuffer.DefaultProcessLimit)
			for turn := 0; turn < 3; turn++ {
				body, _ := json.Marshal(map[string]any{"input": visible})
				budget := pool.Request(256 << 20)
				replay := newJournalReplay(NewConversationJournal(db, journal.cipher, 64<<20))
				restored, prepared, err := replay.Prepare(responsebuffer.WithContext(t.Context(), budget), "synthetic-model", key, body)
				if err != nil {
					t.Fatalf("turn %d: %v; budget=%+v", turn, err, budget.Snapshot())
				}
				if prepared.RestoredItems() != turn {
					prepared.Discard()
					t.Fatalf("turn %d lost reasoning: %d", turn, prepared.RestoredItems())
				}
				if turn == 0 && !bytes.Equal(restored, body) {
					t.Fatal("no-op preparation changed body")
				}
				var parsed struct{ Input []json.RawMessage }
				if err := json.Unmarshal(restored, &parsed); err != nil {
					t.Fatal(err)
				}
				original, _ := json.Marshal(visible[0])
				if !bytes.Equal(parsed.Input[0], original) {
					t.Fatal("media input changed during restoration")
				}
				answer := fmt.Sprintf("synthetic answer %d", turn)
				commitIdentityTurn(t, prepared, fmt.Sprintf("synthetic-response-%d", turn), answer, map[string]any{"type": "reasoning", "encrypted_content": fmt.Sprintf("synthetic-opaque-%d", turn), "summary": []any{}})
				if budget.Snapshot().Used != 0 || pool.Snapshot().Used != 0 {
					t.Fatal("history preparation leaked budget")
				}
				t.Logf("turn %d: input=%d, peak=%d, restored=%d", turn, len(body), budget.Snapshot().Peak, prepared.RestoredItems())
				visible = append(visible, map[string]any{"role": "assistant", "content": answer}, map[string]any{"role": "user", "content": "synthetic continuation"})
			}
		})
	}
}
