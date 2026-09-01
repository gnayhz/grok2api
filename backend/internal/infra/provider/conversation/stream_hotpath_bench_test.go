package conversation

import (
	"bytes"
	"strings"
	"testing"
)

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
	for i := 0; i < b.N; i++ {
		typeName, root, ok := parseSSEEvent("response.output_item.done", data)
		if !ok || typeName != "response.output_item.done" || root != nil {
			b.Fatalf("parse = %q root=%v ok=%v", typeName, root != nil, ok)
		}
	}
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
