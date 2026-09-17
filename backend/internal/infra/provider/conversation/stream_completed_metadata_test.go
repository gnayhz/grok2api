package conversation

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func completedMetadataFrame(padding int) []byte {
	pad := strings.Repeat("x", padding)
	return []byte(`{"type":"response.completed","id":"decoy","model":"decoy","padding_before":"` + pad +
		`","response":{"output":[{"id":"item_decoy","status":"in_progress"}],"id":"resp_actual","model":"grok-4.6","created_at":123,"status":"incomplete","usage":{"input_tokens":123,"output_tokens":45,"total_tokens":168,"input_tokens_details":{"cached_tokens":100},"output_tokens_details":{"reasoning_tokens":12}}},"padding_after":"` + pad + `"}`)
}

func TestCompletedMetadataIndependentOfSizeAndKeyOrder(t *testing.T) {
	for _, operation := range []string{OperationChat, OperationMessages} {
		var baseline []byte
		for _, padding := range []int{0, 40 << 10, 1 << 20} {
			var output bytes.Buffer
			converter := newStreamConverterWithBudget(&output, operation, ResponseOptions{}, nil)
			defer converter.releaseResources()
			converter.created = 7
			if err := converter.handle("", completedMetadataFrame(padding)); err != nil {
				t.Fatal(err)
			}
			if err := converter.finish(); err != nil {
				t.Fatal(err)
			}
			if converter.id != "resp_actual" || converter.model != "grok-4.6" || converter.created != 123 || converter.usage.InputTokens != 123 || converter.usage.InputTokensDetails.CachedTokens != 100 {
				t.Fatalf("%s padding %d: wrong metadata id=%s model=%s created=%d usage=%+v", operation, padding, converter.id, converter.model, converter.created, converter.usage)
			}
			if padding == 0 {
				baseline = bytes.Clone(output.Bytes())
			} else if !bytes.Equal(output.Bytes(), baseline) {
				t.Fatalf("%s padding %d changed completion: %s; want %s", operation, padding, output.Bytes(), baseline)
			}
		}
	}
}

func BenchmarkConvertHugeCompletedMetadata(b *testing.B) {
	data := completedMetadataFrame(1 << 20)
	if !json.Valid(data) {
		b.Fatal("invalid fixture")
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		c := newStreamConverterWithBudget(io.Discard, OperationChat, ResponseOptions{}, nil)
		if err := c.handleHugeCompleted(data, "response.completed"); err != nil {
			b.Fatal(err)
		}
		c.releaseResources()
	}
}
