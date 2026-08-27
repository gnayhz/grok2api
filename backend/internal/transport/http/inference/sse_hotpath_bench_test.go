package inference

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func reasoningDeltaLine() []byte {
	return []byte("data: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"output_index\":0,\"delta\":\"hmm\"}\n")
}

func hugeCiphertextLine(n int) []byte {
	var b strings.Builder
	b.Grow(n + 128)
	b.WriteString("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"")
	b.WriteString(strings.Repeat("A", n))
	b.WriteString("\"}}\n")
	return []byte(b.String())
}

func legacyRewriteResponsesDataLine(line []byte, state *responsesCompatState) []byte {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return line
	}
	changed := sanitizeResponsesEvent(event, state)
	if !changed {
		return line
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return line
	}
	newline := ""
	if bytes.HasSuffix(line, []byte{10}) {
		newline = string([]byte{10})
	}
	return []byte("data: " + string(encoded) + newline)
}

func BenchmarkRewriteReasoningDeltas(b *testing.B) {
	line := reasoningDeltaLine()
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.Run("old", func(b *testing.B) {
		state := &responsesCompatState{}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = legacyRewriteResponsesDataLine(line, state)
		}
	})
	b.Run("new", func(b *testing.B) {
		state := &responsesCompatState{}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = rewriteResponsesDataLine(line, state)
		}
	})
}

func BenchmarkRewriteHugeCiphertext(b *testing.B) {
	line := hugeCiphertextLine(2 << 20)
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.Run("old", func(b *testing.B) {
		state := &responsesCompatState{}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = legacyRewriteResponsesDataLine(line, state)
		}
	})
	b.Run("new", func(b *testing.B) {
		state := &responsesCompatState{}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = rewriteResponsesDataLine(line, state)
		}
	})
}

func BenchmarkGeneratedDeltaDetect(b *testing.B) {
	payload := []byte("{\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"hmm\"}")
	b.ReportAllocs()
	b.Run("old", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var event map[string]any
			if json.Unmarshal(payload, &event) != nil {
				b.Fatal("unmarshal")
			}
			if event["delta"] == "" {
				b.Fatal("expected generated")
			}
		}
	})
	b.Run("new", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !responsesContainsGeneratedDelta(payload) {
				b.Fatal("expected generated")
			}
		}
	})
}

func BenchmarkInspectorReasoningStream(b *testing.B) {
	var body bytes.Buffer
	line := reasoningDeltaLine()
	for i := 0; i < 2000; i++ {
		body.Write(line)
	}
	payload := body.Bytes()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		inspector := &responseInspector{protocol: streamProtocolResponses}
		inspector.Inspect(payload)
	}
}

func BenchmarkInspectorHugeCiphertext(b *testing.B) {
	line := hugeCiphertextLine(2 << 20)
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		inspector := &responseInspector{protocol: streamProtocolResponses}
		inspector.Inspect(line)
	}
}
