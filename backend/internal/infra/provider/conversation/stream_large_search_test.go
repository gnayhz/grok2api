package conversation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCompletedSearchResultsIndependentOfCiphertextSize(t *testing.T) {
	var baseline []byte
	for _, size := range []int{10, 70 << 10, 2 << 20} {
		frame := map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_search", "model": "grok-4.6", "created_at": 123, "status": "completed",
			"output": []any{
				map[string]any{"type": "reasoning", "encrypted_content": strings.Repeat("S", size)},
				map[string]any{"type": "web_search_call", "id": "search_1", "status": "completed", "action": map[string]any{"query": "protocol", "sources": []any{map[string]any{"url": "https://example.com/spec"}}}},
				map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "spec", "annotations": []any{map[string]any{"type": "url_citation", "url": "https://example.com/spec", "title": "Protocol specification"}}}}},
			},
		}}
		data, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		c := newStreamConverter(&output, OperationMessages, ResponseOptions{AnthropicWebSearch: true})
		if err := c.handle("response.completed", data); err != nil {
			t.Fatal(err)
		}
		if err := c.finish(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(output.Bytes(), []byte("Protocol specification")) || !bytes.Contains(output.Bytes(), []byte("web_search_tool_result")) {
			t.Fatalf("size %d lost search results: %s", size, output.Bytes())
		}
		if baseline == nil {
			baseline = bytes.Clone(output.Bytes())
		} else if !bytes.Equal(output.Bytes(), baseline) {
			t.Fatalf("size %d changed search output: %s", size, output.Bytes())
		}
		c.releaseResources()
	}
}
