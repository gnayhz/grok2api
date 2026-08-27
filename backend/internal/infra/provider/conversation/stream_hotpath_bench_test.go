package conversation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func legacyParseSSEEvent(event string, data []byte) (string, map[string]json.RawMessage, bool) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return "", nil, false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return "", nil, false
	}
	typeName := event
	if typeName == "" {
		_ = json.Unmarshal(root["type"], &typeName)
	}
	return typeName, root, true
}

func hugeItemDoneJSON(n int) []byte {
	var b strings.Builder
	b.Grow(n + 128)
	b.WriteString("{\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"")
	b.WriteString(strings.Repeat("A", n))
	b.WriteString("\"}}")
	return []byte(b.String())
}

func BenchmarkParseHugeOutputItemDone(b *testing.B) {
	data := hugeItemDoneJSON(2 << 20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.Run("old", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, root, ok := legacyParseSSEEvent("response.output_item.done", data)
			if !ok || root == nil {
				b.Fatal("old parse should materialize map")
			}
		}
	})
	b.Run("new", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			typeName, root, ok := parseSSEEvent("response.output_item.done", data)
			if !ok || typeName != "response.output_item.done" || root != nil {
				b.Fatalf("new parse = %q root=%v ok=%v", typeName, root != nil, ok)
			}
		}
	})
}

func BenchmarkConsumeSSEReasoningDeltas(b *testing.B) {
	var body strings.Builder
	for i := 0; i < 2000; i++ {
		body.WriteString("event: response.reasoning_text.delta\n")
		body.WriteString("data: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"hmm\"}\n\n")
	}
	payload := []byte(body.String())
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		count := 0
		if err := consumeSSE(bytes.NewReader(payload), func(string, []byte) error {
			count++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if count != 2000 {
			b.Fatalf("events = %d", count)
		}
	}
}
