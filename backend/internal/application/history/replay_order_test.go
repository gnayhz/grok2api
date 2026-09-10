package history

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestReplayUsesHistoryOrderWhenCompletionsArriveOutOfOrder(t *testing.T) {
	for _, tools := range []bool{false, true} {
		t.Run(fmt.Sprint(tools), func(t *testing.T) {
			replay := New(memory.NewReasoningReplayStore(4), Config{Enabled: true, TTL: time.Hour}, nil)
			ctx := context.Background()
			var history []any
			history = append(history, map[string]any{"type": "message", "role": "user", "content": "start"})
			payloads := make([][]byte, 3)
			ciphers := make([]string, 3)
			for i := 0; i < 3; i++ {
				ciphers[i] = validEncrypted(byte(230 + i))
				var anchor map[string]any
				if tools {
					anchor = map[string]any{"type": "function_call", "call_id": fmt.Sprintf("call-%d", i), "name": "inspect", "arguments": "{}"}
				} else {
					anchor = map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("answer %d", i)}}}
				}
				payloads[i], _ = json.Marshal(map[string]any{"output": []any{map[string]any{"type": "reasoning", "encrypted_content": ciphers[i]}, anchor}})
				history = append(history, anchor)
				if tools {
					history = append(history, map[string]any{"type": "function_call_output", "call_id": fmt.Sprintf("call-%d", i), "output": "ok"})
				}
				history = append(history, map[string]any{"type": "message", "role": "user", "content": "continue"})
			}
			for _, i := range []int{2, 0, 1} {
				replay.StoreFromCompleted(ctx, "m", "s", payloads[i])
			}
			body, _ := json.Marshal(map[string]any{"input": history})
			out := replay.Apply(ctx, "m", "s", body)
			var got struct {
				Input []struct {
					Type   string `json:"type"`
					Cipher string `json:"encrypted_content"`
				} `json:"input"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			pos := 0
			for _, item := range got.Input {
				if item.Type == "reasoning" {
					if pos >= 3 || item.Cipher != ciphers[pos] {
						t.Fatal("reasoning order differs from client history")
					}
					pos++
				}
			}
			if pos != 3 {
				t.Fatalf("lost restoration after out-of-order completion: got %d of 3 turns", pos)
			}
		})
	}
}

func TestMalformedInputDoesNotShiftReplayInsertion(t *testing.T) {
	items := [][]byte{[]byte(`{"type":"reasoning","encrypted_content":"` + validEncrypted(235) + `"}`), []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)}
	body := []byte(`{"input":[42,{"type":"message","role":"user","content":"question"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`)
	if _, changed := insertReplayItems(body, items); changed {
		t.Fatal("malformed input was partially decoded and replay positions shifted")
	}
}
