package inference

import (
	"strings"
	"testing"
)

var sizeEquivCipherBytes = []int{4 << 10, 63 << 10, 65 << 10, 2 << 20}

func sizeEquivUpstream(cipher string) []byte {
	return []byte(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6","sequence_number":0}}`,
		`data: {"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"plan","sequence_number":1}`,
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}}",
		`data: {"type":"response.output_text.delta","delta":"answer","sequence_number":3}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.6","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":12,"output_tokens":95,"output_tokens_details":{"reasoning_tokens":40},"cost_in_usd_ticks":99}}}`,
		"",
	}, "\n"))
}

type inspectorSizeSnap struct {
	err      string
	id       string
	model    string
	seq      int64
	in       int64
	out      int64
	reason   int64
	cost     int64
	observed bool
	events   int64
}

func inspectSizeSnap(body []byte, chunk int) inspectorSizeSnap {
	inspector := &responseInspector{protocol: streamProtocolResponses}
	if chunk <= 0 || chunk >= len(body) {
		inspector.Inspect(body)
	} else {
		for i := 0; i < len(body); i += chunk {
			end := i + chunk
			if end > len(body) {
				end = len(body)
			}
			inspector.Inspect(body[i:end])
		}
	}
	inspector.Finish()
	meta := inspector.Metadata()
	err := ""
	if e := inspector.TerminalError(); e != nil {
		err = e.Error()
	}
	return inspectorSizeSnap{
		err: err, id: meta.ResponseID, model: meta.Model, seq: meta.SequenceNumber,
		in: meta.Usage.InputTokens, out: meta.Usage.OutputTokens, reason: meta.Usage.ReasoningTokens,
		cost: meta.Usage.CostInUSDTicks, observed: meta.Usage.OutputObserved, events: meta.DeliveredEvents,
	}
}

func TestInspectorSizeEquivalence(t *testing.T) {
	t.Parallel()
	baseline := inspectSizeSnap(sizeEquivUpstream(strings.Repeat("A", sizeEquivCipherBytes[0])), 32<<10)
	if baseline.err != "" || baseline.id != "resp_1" || baseline.out != 95 || baseline.reason != 40 || baseline.cost != 99 || !baseline.observed {
		t.Fatalf("baseline = %+v", baseline)
	}
	for _, size := range sizeEquivCipherBytes[1:] {
		got := inspectSizeSnap(sizeEquivUpstream(strings.Repeat("A", size)), 32<<10)
		if got != baseline {
			t.Fatalf("cipher %d: %+v want %+v", size, got, baseline)
		}
	}
}
