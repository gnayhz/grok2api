package gateway

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

var sizeEquivCipherBytes = []int{4 << 10, 63 << 10, 65 << 10, 2 << 20}

func sizeEquivQualityBody(cipher string) []byte {
	return []byte(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6"}}`,
		`data: {"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"plan"}`,
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}}",
		`data: {"type":"response.output_text.delta","delta":"answer"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":12,"output_tokens":95,"output_tokens_details":{"reasoning_tokens":40},"cost_in_usd_ticks":99}}}`,
		"",
	}, "\n"))
}

type qualitySizeSnap struct {
	terminal  bool
	thinking  bool
	id        string
	out       int64
	reason    int64
	cost      int64
	visible   int
	aggregate int
}

func qualitySizeSnapFrom(body []byte, chunk int) qualitySizeSnap {
	state := qualityScanState{protocol: qualityProtocolResponses}
	if chunk <= 0 || chunk >= len(body) {
		observeQualityChunk(&state, body)
	} else {
		for i := 0; i < len(body); i += chunk {
			end := i + chunk
			if end > len(body) {
				end = len(body)
			}
			observeQualityChunk(&state, body[i:end])
		}
	}
	if len(state.pending) > 0 || state.skipUntilNewline {
		observeQualityChunk(&state, []byte{10})
	}
	return qualitySizeSnap{
		terminal: state.terminal, thinking: state.hasThinking, id: state.responseID,
		out: state.usage.OutputTokens, reason: state.usage.ReasoningTokens, cost: state.usage.CostInUSDTicks,
		visible: state.visibleRunes, aggregate: state.aggregateRunes,
	}
}

func TestQualityScannerSizeEquivalence(t *testing.T) {
	t.Parallel()
	baseline := qualitySizeSnapFrom(sizeEquivQualityBody(strings.Repeat("A", sizeEquivCipherBytes[0])), 32<<10)
	if !baseline.terminal || !baseline.thinking || baseline.id != "resp_1" || baseline.out != 95 || baseline.reason != 40 || baseline.cost != 99 || baseline.visible == 0 {
		t.Fatalf("baseline = %+v", baseline)
	}
	for _, size := range sizeEquivCipherBytes[1:] {
		got := qualitySizeSnapFrom(sizeEquivQualityBody(strings.Repeat("A", size)), 32<<10)
		if got.terminal != baseline.terminal || got.thinking != baseline.thinking || got.id != baseline.id || got.out != baseline.out || got.reason != baseline.reason || got.cost != baseline.cost || got.visible != baseline.visible {
			t.Fatalf("cipher %d: %+v want %+v", size, got, baseline)
		}
	}
}

func TestQualityReadPumpDoubleBufferPreservesBytes(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("abcdefghijklmnop"), 9000)
	pump := newQualityReadPump(io.NopCloser(bytes.NewReader(payload)))
	defer pump.Close()
	var got []byte
	buf := make([]byte, 1000)
	for {
		n, err := pump.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("pump reconstructed %d bytes, want %d", len(got), len(payload))
	}
}
