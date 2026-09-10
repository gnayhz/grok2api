package conversation

import (
	"bytes"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

func TestStreamEventTypedDeltaDecoding(t *testing.T) {
	for _, test := range []struct{ name, event, data, kind, delta string }{
		{"escaped", "", `{"type":"response.output_text.delta","delta":"hello\n\u4e16\u754c","item_id":"msg_1"}`, "response.output_text.delta", "hello\n世界"},
		{"sse type override", "response.reasoning_text.delta", `{"type":"response.output_text.delta","delta":"thought"}`, "response.reasoning_text.delta", "thought"},
		{"duplicate", "", `{"delta":"old","type":"response.output_text.delta","delta":"new"}`, "response.output_text.delta", "new"},
		{"escaped keys", "", `{"t\u0079pe":"response.output_text.delta","d\u0065lta":"value"}`, "response.output_text.delta", "value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			kind, frame, ok := parseSSEEvent(test.event, []byte(test.data))
			if !ok || kind != test.kind || frame.Delta != test.delta {
				t.Fatalf("kind=%q frame=%+v ok=%v", kind, frame, ok)
			}
		})
	}
}

func TestCompletedEncoderIgnoresTrailingStateWithoutReservation(t *testing.T) {
	var output bytes.Buffer
	budget := responsebuffer.NewPool(1 << 20).Request(1 << 20)
	c := newStreamConverterWithBudget(&output, OperationMessages, ResponseOptions{}, budget)
	defer c.releaseResources()
	if err := c.handle("response.completed", []byte(`{"type":"response.completed","response":{"status":"completed"}}`)); err != nil {
		t.Fatal(err)
	}
	before := budget.Snapshot()
	length := output.Len()
	if err := c.handle("response.output_item.added", []byte(strings.Repeat("x", 2<<20))); err != nil {
		t.Fatalf("finished encoder reserved unused trailing event: %v", err)
	}
	if output.Len() != length || budget.Snapshot() != before {
		t.Fatal("finished encoder retained or emitted trailing event")
	}
}
