package gateway

import (
	"strings"
	"testing"
)

// benchStream builds a realistic healthy A-form stream: reasoning delta,
// many content deltas, usage frame, DONE.
func benchStream(frames int) string {
	var b strings.Builder
	b.WriteString(": grok2api-reasoning-start\n\n")
	b.WriteString("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan the answer\"}}]}\n\n")
	for i := 0; i < frames; i++ {
		b.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"word word word word word word word word\"}}]}\n\n")
	}
	b.WriteString("data: {\"usage\":{\"completion_tokens\":400,\"completion_tokens_details\":{\"reasoning_tokens\":120}}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func BenchmarkObserveQualityChunk(b *testing.B) {
	payload := []byte(benchStream(500))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state := &qualityScanState{protocol: qualityProtocolChat}
		chunk := payload
		for len(chunk) > 0 {
			n := min(4096, len(chunk))
			ObserveQualityChunk(state, chunk[:n])
			chunk = chunk[n:]
		}
		if !state.hasThinking || state.visibleRunes == 0 {
			b.Fatal("benchmark must observe a healthy stream")
		}
	}
}

func BenchmarkSignals(b *testing.B) {
	state := &qualityScanState{protocol: qualityProtocolChat, visibleRunes: 2000, hasThinking: true}
	state.usage = Usage{Reported: true, OutputTokens: 500, ReasoningTokens: 120}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ClassifyQualityHold(state.signals(), 32)
	}
}
