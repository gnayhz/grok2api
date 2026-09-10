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
