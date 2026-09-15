package history

import (
	"bytes"
	"fmt"
	"testing"
)

func TestJournalTerminalCannotDropIndexedReasoning(t *testing.T) {
	cipher := validEncrypted(19)
	reasoning := `{"type":"reasoning","encrypted_content":"` + cipher + `"}`
	assistant := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	data := fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n"+"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":%s}\n"+"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[%s]}}\n", reasoning, assistant, assistant)
	payload, err := extractJournalPayloadFromSSE([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(cipher)) {
		t.Fatal("terminal silently discarded reasoning")
	}
	incomplete := bytes.ReplaceAll([]byte(data), []byte(`"output_index":0`), []byte(`"output_index":9`))
	if _, err = extractJournalPayloadFromSSE(incomplete); err == nil {
		t.Fatal("ambiguous output order accepted")
	}
}

// Short native opaque strings remain distinct even when a legacy cache would
// refuse to replay either one. An index conflict must never collapse to "".
func TestJournalTerminalRejectsConflictingShortReasoning(t *testing.T) {
	first := `{"type":"reasoning","encrypted_content":"synthetic-opaque-a"}`
	second := `{"type":"reasoning","encrypted_content":"synthetic-opaque-b"}`
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			data := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + first + "}\n"
			if !terminal {
				data += "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + second + "}\n"
			}
			data += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic-response\",\"status\":\"completed\",\"output\":[" + second + "]}}\n"
			if _, err := extractJournalPayloadFromSSE([]byte(data)); err == nil {
				t.Fatal("conflicting opaque reasoning was silently accepted")
			}
		})
	}
}
