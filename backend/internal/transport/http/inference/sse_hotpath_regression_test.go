package inference

import (
	"bytes"
	"strings"
	"testing"
)

func TestInspectorHugeCompletedIsTerminalWithUsage(t *testing.T) {
	cipher := strings.Repeat("A", maxParsedSSEJSONBytes+64)
	line := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.6\",\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}],\"usage\":{\"output_tokens\":5000,\"output_tokens_details\":{\"reasoning_tokens\":4000}}}}\n")
	inspector := &responseInspector{protocol: streamProtocolResponses}
	inspector.Inspect(line)
	inspector.Finish()
	if err := inspector.TerminalError(); err != nil {
		t.Fatalf("huge completed treated as incomplete: %v", err)
	}
	meta := inspector.Metadata()
	if meta.ResponseID != "resp_1" {
		t.Fatalf("id = %q", meta.ResponseID)
	}
	if meta.Usage.OutputTokens != 5000 || meta.Usage.ReasoningTokens != 4000 {
		t.Fatalf("usage output=%d reasoning=%d", meta.Usage.OutputTokens, meta.Usage.ReasoningTokens)
	}
	if !meta.Usage.OutputObserved {
		t.Fatal("output not observed")
	}
}

func TestRewriteFillsAnnotationsOnLargeTextWithoutCiphertext(t *testing.T) {
	text := strings.Repeat("word ", 20<<10)
	line := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"" + text + "\"}]}}\n")
	got := rewriteResponsesDataLine(line, &responsesCompatState{})
	if !bytes.Contains(got, []byte("\"annotations\":[]")) {
		t.Fatalf("large text item lost annotations: %s", got[:min(len(got), 200)])
	}
}

func TestInspectorDoesNotStickItemIDAsResponseID(t *testing.T) {
	inspector := &responseInspector{protocol: streamProtocolResponses}
	inspector.Inspect([]byte(`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}` + "\n"))
	inspector.Inspect([]byte(`data: {"type":"response.completed","response":{"id":"resp_real","model":"grok-4.6","usage":{"output_tokens":1}}}` + "\n"))
	inspector.Finish()
	meta := inspector.Metadata()
	if meta.ResponseID != "resp_real" {
		t.Fatalf("ResponseID = %q, item id must not stick", meta.ResponseID)
	}
	if meta.Model != "grok-4.6" {
		t.Fatalf("model = %q", meta.Model)
	}
}
