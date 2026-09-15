package gateway

import (
	"bytes"
	"testing"
)

func FuzzGuardDecisionPartitions(f *testing.F) {
	f.Add("responses", []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\ndata: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"late\"}\n\n"), uint16(7))
	f.Add("chat", []byte("data: {\"usage\":{\"completion_tokens\":40}}\n\ndata: [DONE]\n\n"), uint16(1))
	f.Add("anthropic", []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n"), uint16(11))
	f.Fuzz(func(t *testing.T, protocol string, data []byte, split uint16) {
		if len(data) > 1<<20 {
			return
		}
		type outcome struct {
			verdict   QualityVerdict
			rule, err string
		}
		run := func(step int) outcome {
			state := qualityScanState{protocol: protocol}
			cfg := QualityRetryRuntime{ReasoningExpected: true}
			var held bytes.Buffer
			offset := 0
			verdict := QualityWait
			var err error
			for pos := 0; pos < len(data); {
				n := min(step, len(data)-pos)
				searched := held.Len() - offset
				held.Write(data[pos : pos+n])
				pos += n
				consumed, v, e := scanQualityLines(&state, held.Bytes()[offset:], searched, &cfg)
				offset += consumed
				if v != QualityWait || e != nil {
					verdict, err = v, e
					break
				}
			}
			if verdict == QualityWait && err == nil {
				observeQualityLine(&state, held.Bytes()[offset:])
				state.terminal = true
				verdict, err = state.streamVerdict(true)
			}
			message := ""
			if err != nil {
				message = err.Error()
			}
			return outcome{verdict, qualityHoldRule(state.signals(), state.semanticOutput, err), message}
		}
		whole := run(max(1, len(data)))
		fragmented := run(int(split%1024) + 1)
		if whole != fragmented {
			t.Fatalf("whole=%+v fragmented=%+v input=%q", whole, fragmented, data)
		}
	})
}
