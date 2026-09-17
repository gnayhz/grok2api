package conversation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLargeReasoningItemsPreserveIdentityAndSignature(t *testing.T) {
	var output bytes.Buffer
	c := newStreamConverterWithBudget(&output, OperationMessages, ResponseOptions{AnthropicThinking: true}, nil)
	defer c.releaseResources()
	signature := strings.Repeat("S", 70<<10)
	// Identity and type deliberately follow ciphertext, beyond any head window.
	item := `{"encrypted_content":"` + signature + `","id":"rs_real","type":"reasoning"}`
	for _, kind := range []string{"response.output_item.added", "response.output_item.done"} {
		data := []byte(`{"type":"` + kind + `","encrypted_content":"decoy","item":` + item + `}`)
		if err := c.handle(kind, data); err != nil {
			t.Fatal(err)
		}
	}
	if c.thinkingItemID != "rs_real" || !c.thinkingClosed {
		t.Fatalf("large reasoning item lost: id=%q closed=%v", c.thinkingItemID, c.thinkingClosed)
	}
	if got := anthropicSignature(t, output.Bytes()); got != signature {
		t.Fatalf("signature size=%d, want %d", len(got), len(signature))
	}
}

func TestLargeFunctionItemsPreserveToolArguments(t *testing.T) {
	for _, operation := range []string{OperationChat, OperationMessages} {
		var output bytes.Buffer
		c := newStreamConverterWithBudget(&output, operation, ResponseOptions{}, nil)
		defer c.releaseResources()
		arguments := `{"text":"` + strings.Repeat("A", 70<<10) + `"}`
		item := map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "write", "arguments": arguments}
		for _, kind := range []string{"response.output_item.added", "response.output_item.done"} {
			data, err := json.Marshal(map[string]any{"type": kind, "item": item})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.handle(kind, data); err != nil {
				t.Fatal(err)
			}
		}
		var received strings.Builder
		for _, line := range bytes.Split(output.Bytes(), []byte{'\n'}) {
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			var frame struct {
				Delta struct {
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
				Choices []struct {
					Delta struct {
						ToolCalls []struct {
							Function struct {
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(line[6:], &frame); err != nil {
				t.Fatal(err)
			}
			received.WriteString(frame.Delta.PartialJSON)
			for _, choice := range frame.Choices {
				for _, call := range choice.Delta.ToolCalls {
					received.WriteString(call.Function.Arguments)
				}
			}
		}
		if received.String() != arguments {
			t.Fatalf("%s received %d argument bytes, want %d", operation, received.Len(), len(arguments))
		}
	}
}
