package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	reasoningreplay "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// Switching from server-restored reasoning to client-supplied reasoning must
// not rewrite an earlier prefix. Both paths use the official input fields.
func TestRestoredReasoningMatchesClientHistoryInput(t *testing.T) {
	ctx := context.Background()
	var cipherBytes [256]byte
	for i := range cipherBytes {
		cipherBytes[i] = byte(i)
	}
	encrypted := base64.RawStdEncoding.EncodeToString(cipherBytes[:])
	outputItem := map[string]any{"type": "reasoning", "id": "rs_cache", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": "keep this summary"}}, "content": nil, "encrypted_content": encrypted, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "upstream-output-only"}}
	assistant := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}}
	payload, _ := json.Marshal(map[string]any{"output": []any{outputItem, assistant}})
	body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "content": "question"}, assistant, map[string]any{"role": "user", "content": "continue"}}})
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new_capture", true: "existing_store"}[legacy], func(t *testing.T) {
			store := memory.NewReasoningReplayStore(8)
			replay := reasoningreplay.New(store, reasoningreplay.Config{Enabled: true, TTL: time.Hour}, nil)
			if legacy {
				rawReasoning, _ := json.Marshal(outputItem)
				rawAssistant, _ := json.Marshal(assistant)
				if err := store.Set(ctx, "grok-4.6", "cache-session", [][]byte{rawReasoning, rawAssistant}, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			} else {
				replay.StoreFromCompleted(ctx, "grok-4.6", "cache-session", payload)
			}
			var restored struct {
				Input []map[string]any `json:"input"`
			}
			if err := json.Unmarshal(replay.Apply(ctx, "grok-4.6", "cache-session", body), &restored); err != nil {
				t.Fatal(err)
			}
			if len(restored.Input) != 4 {
				t.Fatalf("input item count=%d", len(restored.Input))
			}
			want := sanitizeReasoningInput(outputItem)
			if !reflect.DeepEqual(restored.Input[1], want) {
				t.Fatal("restored reasoning differs from client history input")
			}
			if restored.Input[1]["encrypted_content"] != encrypted {
				t.Fatal("encrypted content changed")
			}
		})
	}
}
