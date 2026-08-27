package conversation

import (
	"bytes"
	"strings"
	"testing"
)

func TestHugeReasoningDoneSuppressedAfterStop(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	converter := newStreamConverter(&out, OperationMessages, ResponseOptions{AnthropicThinking: true, StopSequences: []string{"STOP"}})
	if err := converter.handle("response.output_item.added", []byte("{\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}")); err != nil {
		t.Fatal(err)
	}
	if err := converter.handle("response.output_text.delta", []byte("{\"type\":\"response.output_text.delta\",\"delta\":\"STOP\"}")); err != nil {
		t.Fatal(err)
	}
	cipher := strings.Repeat("C", maxParsedSSEJSONBytes+32)
	data := []byte("{\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}}")
	if err := converter.handle("response.output_item.done", data); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("signature_delta")) {
		t.Fatal("signature_delta emitted past stop filter")
	}
}
