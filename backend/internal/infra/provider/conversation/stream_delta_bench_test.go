package conversation

import (
	"io"
	"testing"
)

func BenchmarkConvertTextDeltas(b *testing.B) {
	for _, operation := range []string{OperationChat, OperationMessages} {
		b.Run(operation, func(b *testing.B) {
			data := [2][]byte{
				[]byte(`{"type":"response.output_text.delta","item_id":"msg_1","delta":"hello 世界","sequence_number":123}`),
				[]byte(`{"type":"response.output_text.delta","item_id":"msg_1","delta":" next","sequence_number":124}`),
			}
			c := newStreamConverter(io.Discard, operation, ResponseOptions{})
			defer c.releaseResources()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if err := c.handle("", data[i%2]); err != nil {
					b.Fatal(err)
				}
				i++
			}
		})
	}
}

func BenchmarkParseTextDelta(b *testing.B) {
	data := []byte(`{"type":"response.output_text.delta","item_id":"msg_1","delta":"hello 世界","sequence_number":123}`)
	b.ReportAllocs()
	for b.Loop() {
		_, _, ok := parseSSEEvent("", data)
		if !ok {
			b.Fatal("not parsed")
		}
	}
}
