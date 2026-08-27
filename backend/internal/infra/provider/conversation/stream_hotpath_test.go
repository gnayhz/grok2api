package conversation

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestConsumeSSEReadsLongLines(t *testing.T) {
	t.Parallel()
	delta := strings.Repeat("thought ", 20<<10)
	stream := strings.Join([]string{
		"event: response.reasoning_text.delta",
		"data: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"" + delta + "\"}",
		"",
	}, string([]byte{10}))
	var got []string
	if err := consumeSSE(strings.NewReader(stream), func(event string, data []byte) error {
		got = append(got, event+"|"+string(data[:min(len(data), 80)]))
		if !bytes.Contains(data, []byte(delta[:32])) {
			t.Fatal("long delta was truncated")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "response.reasoning_text.delta|") {
		t.Fatalf("events = %v", got)
	}
}

func TestConvertChatKeepsReasoningAcrossHugeEncryptedItem(t *testing.T) {
	t.Parallel()
	cipher := strings.Repeat("B", maxParsedSSEJSONBytes+4096)
	stream := strings.Join([]string{
		"event: response.created",
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.6\"}}",
		"",
		"event: response.reasoning_text.delta",
		"data: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"plan\"}",
		"",
		"event: response.output_item.done",
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}}",
		"",
		"event: response.output_text.delta",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}",
		"",
		"event: response.completed",
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}",
		"",
		"",
	}, string([]byte{10}))
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, "\"reasoning_content\":\"plan\"") {
		t.Fatalf("thinking delta lost: %s", text)
	}
	if !strings.Contains(text, "\"content\":\"answer\"") {
		t.Fatalf("answer lost: %s", text)
	}
	if strings.Contains(text, cipher[:32]) {
		t.Fatal("chat conversion leaked encrypted_content")
	}
}
