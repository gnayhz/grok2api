package gateway

import "testing"

// FuzzObserveQualityChunk feeds arbitrary bytes into the quality scanner for
// all three protocols. Invariants: no panic on any chunk, counters never go
// negative, and classification is safe on every intermediate state. Split
// feeding also exercises the line-accumulation path (partial frames).
func FuzzObserveQualityChunk(f *testing.F) {
	f.Add("chat", []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n"), uint8(3))
	f.Add("chat", []byte("data: [DONE]\n\n"), uint8(1))
	f.Add("chat", []byte(": comment\n\ndata: not-json\n\n"), uint8(7))
	f.Add("chat", []byte("data: {\"usage\":{\"completion_tokens\":1e999}}"), uint8(2))
	f.Add("chat", []byte("\xff\xfe broken \x00\n"), uint8(5))
	f.Add("responses", []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ans\"}"), uint8(4))
	f.Add("anthropic", []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"h\"}}"), uint8(6))
	f.Fuzz(func(t *testing.T, protocol string, chunk []byte, splits uint8) {
		switch protocol {
		case qualityProtocolChat, qualityProtocolResponses, qualityProtocolAnthropic:
		default:
			protocol = qualityProtocolChat
		}
		state := &qualityScanState{protocol: protocol}
		parts := int(splits%3) + 1
		step := (len(chunk) + parts - 1) / parts
		if step < 1 {
			step = 1
		}
		for start := 0; start < len(chunk); start += step {
			end := start + step
			if end > len(chunk) {
				end = len(chunk)
			}
			observeQualityChunk(state, chunk[start:end])
			signals := state.signals()
			if signals.VisibleTokens < 0 || signals.OutputTokens < 0 || signals.ReasoningTokens < 0 {
				t.Fatalf("negative counters after %q: %#v", chunk[start:end], signals)
			}
			_ = classifyQualityHold(signals)
		}
		observeQualityChunk(state, nil) // nil must be a no-op
	})
}
