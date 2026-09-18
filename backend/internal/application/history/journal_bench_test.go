package history

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Large inline media must not be encoded repeatedly just to classify history
// items. Keep both restoration and the no-change continuation path measurable.
func BenchmarkRestoreJournalMediaPrefix(b *testing.B) {
	media, _ := json.Marshal(map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("A", 1<<20)}}})
	reasoning := json.RawMessage(`{"type":"reasoning","encrypted_content":"synthetic-opaque","summary":[]}`)
	assistant := json.RawMessage(`{"role":"assistant","content":"synthetic answer"}`)
	next := json.RawMessage(`{"role":"user","content":"synthetic next"}`)
	turns := []repository.JournalTurn{{InputCount: 1, TotalCount: 2, Input: [][]byte{media}, Output: [][]byte{reasoning, assistant}}}
	for _, carried := range []bool{false, true} {
		name := "restore"
		input := []json.RawMessage{media, assistant, next}
		if carried {
			name = "already_present"
			input = []json.RawMessage{media, reasoning, assistant, next}
		}
		b.Run(name, func(b *testing.B) {
			root := map[string]json.RawMessage{}
			b.ReportAllocs()
			b.SetBytes(int64(len(media)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, restored, err := restoreJournalItems(root, input, turns)
				if err != nil || carried && restored != 0 || !carried && restored != 1 {
					b.Fatalf("restore=%d error=%v", restored, err)
				}
			}
		})
	}
}
