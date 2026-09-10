package inference

import (
	"bytes"
	"testing"
)

func TestResponsesCompatibilityPreservesNumbers(t *testing.T) {
	for _, tail := range []bool{false, true} {
		state := new(responsesCompatState)
		line := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","metadata":{"sequence":9007199254740993,"ratio":0.12345678901234567890123456789},"output":[]}}`)
		var output []byte
		if tail {
			state.pending = line
			output = flushResponsesStreamTail(state)
		} else {
			output = rewriteResponsesDataLine(line, state)
		}
		for _, number := range []string{"9007199254740993", "0.12345678901234567890123456789"} {
			if !bytes.Contains(output, []byte(number)) {
				t.Fatalf("tail=%v lost %s: %s", tail, number, output)
			}
		}
	}
}
