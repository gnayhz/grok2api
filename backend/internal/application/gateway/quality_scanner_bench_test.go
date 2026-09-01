package gateway

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
)

// benchStream builds a realistic healthy A-form stream: reasoning delta,
// many content deltas, usage frame, DONE.
func benchStream(frames int) string {
	var b strings.Builder
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
			observeQualityChunk(state, chunk[:n])
			chunk = chunk[n:]
		}
		if !state.hasThinking || state.visibleRunes == 0 {
			b.Fatal("benchmark must observe a healthy stream")
		}
	}
}

// BenchmarkZeroDelayWithholdPeek locks the blueprint SLA (interception
// latency < 0.01s) as a tracked number: end-to-end peek time from feeding
// a ciphertext item.done degraded stream to the QualityWithhold verdict,
// across ciphertext sizes (the huge-line skip path engages past 1MiB).
func BenchmarkZeroDelayWithholdPeek(b *testing.B) {
	for _, cipherKiB := range []int{16, 64, 256} {
		b.Run(strconv.Itoa(cipherKiB)+"KiB", func(b *testing.B) {
			ciphertext := `"encrypted_content":"` + strings.Repeat("A", cipherKiB*1024) + `"`
			degraded := strings.Join([]string{
				"data: " + `{"type":"response.created","response":{"id":"resp_deg"}}`,
				"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","` + ciphertext[1:] + `}}`,
			}, "\n")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(degraded)), qualityProtocolResponses, QualityRetryRuntime{})
				if err != nil || verdict != QualityWithhold {
					b.Fatalf("verdict=%s err=%v", verdict, err)
				}
				_ = replay.Close()
			}
		})
	}
}

func benchResponsesStream(deltas int) string {
	var b strings.Builder
	write := func(payload string) {
		b.WriteString("data: ")
		b.WriteString(payload)
		b.WriteString("\n\n")
	}
	write(`{"type":"response.created","response":{"id":"resp_ok"}}`)
	write(`{"type":"response.in_progress"}`)
	write(`{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`)
	write(`{"type":"response.reasoning_summary_part.added"}`)
	for i := 0; i < deltas; i++ {
		write(`{"type":"response.reasoning_summary_text.delta","delta":"step"}`)
	}
	write(`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning"}}`)
	write(`{"type":"response.output_item.added","item":{"id":"msg_1","type":"message"}}`)
	write(`{"type":"response.content_part.added"}`)
	for i := 0; i < deltas; i++ {
		write(`{"type":"response.output_text.delta","delta":"word "}`)
	}
	write(`{"type":"response.completed","response":{"usage":{"output_tokens":400,"output_tokens_details":{"reasoning_tokens":120}}}}`)
	return b.String()
}

func BenchmarkObserveQualityResponses(b *testing.B) {
	payload := []byte(benchResponsesStream(250))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state := &qualityScanState{protocol: qualityProtocolResponses}
		chunk := payload
		for len(chunk) > 0 {
			n := min(4096, len(chunk))
			observeQualityChunk(state, chunk[:n])
			chunk = chunk[n:]
		}
		if !state.hasThinking || state.visibleRunes == 0 {
			b.Fatal("benchmark must observe a healthy responses stream")
		}
	}
}

func benchAnthropicStream(deltas int) string {
	var b strings.Builder
	write := func(payload string) {
		b.WriteString("data: ")
		b.WriteString(payload)
		b.WriteString("\n\n")
	}
	write(`{"type":"message_start","message":{"id":"msg_ok"}}`)
	write(`{"type":"content_block_start","content_block":{"type":"thinking"}}`)
	for i := 0; i < deltas; i++ {
		write(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"step"}}`)
	}
	write(`{"type":"content_block_stop"}`)
	write(`{"type":"content_block_start","content_block":{"type":"text"}}`)
	for i := 0; i < deltas; i++ {
		write(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"word "}}`)
	}
	write(`{"type":"content_block_stop"}`)
	write(`{"type":"message_stop"}`)
	return b.String()
}

func BenchmarkObserveQualityAnthropic(b *testing.B) {
	payload := []byte(benchAnthropicStream(250))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state := &qualityScanState{protocol: qualityProtocolAnthropic}
		chunk := payload
		for len(chunk) > 0 {
			n := min(4096, len(chunk))
			observeQualityChunk(state, chunk[:n])
			chunk = chunk[n:]
		}
		if !state.hasThinking || state.visibleRunes == 0 {
			b.Fatal("benchmark must observe a healthy anthropic stream")
		}
	}
}

func BenchmarkSignals(b *testing.B) {
	state := &qualityScanState{protocol: qualityProtocolChat, visibleRunes: 2000, hasThinking: true}
	state.usage = Usage{Reported: true, OutputTokens: 500, ReasoningTokens: 120}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = classifyQualityHold(state.signals())
	}
}
