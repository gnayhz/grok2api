package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestHoldExpiredIsSticky：hold 超时后到达的小输出必须立即扣留，而不是退回 Wait 等待流关闭
// ——否则慢速降智流把首字节无限推迟（审查发现的 P1）。
func TestHoldExpiredIsSticky(t *testing.T) {
	t.Parallel()
	// Bounded peek context: a stickiness regression must fail fast here
	// instead of hanging the suite until its global timeout.
	peekCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		time.Sleep(120 * time.Millisecond)
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\" more\"}}]}\n\n"))
		// Functional tail: if stickiness regressed, peek would fall through
		// to this EOF — the elapsed bound below must discriminate
		// sticky-expiry from stream-close. Kept open well past the bound.
		time.Sleep(600 * time.Millisecond)
	}()
	started := time.Now()
	replay, verdict, _, _, err := peekQualityStream(peekCtx, reader, qualityProtocolChat,
		QualityRetryRuntime{MinOutputTokens: 4096, HoldTimeout: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("verdict = %s, want withhold right after post-timeout deltas", verdict)
	}
	if elapsed := time.Since(started); elapsed > 350*time.Millisecond {
		t.Fatalf("sticky hold-expiry should decide on the first delta after timeout, took %s", elapsed)
	}
	// Join the writer so no goroutine outlives the test.
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine must join before test end")
	}
}

// TestUsageOnlyStreamIsEmpty：只有 usage 帧（声称 reasoning tokens）、零内容零推理事件的流是空流，
// usage 声明不能把空 200 洗成可投递响应。
func TestUsageOnlyStreamIsEmpty(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":12,"completion_tokens_details":{"reasoning_tokens":920}}}`,
		"data: [DONE]",
		"",
	}, "\n")
	replay, verdict, _, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolChat,
		QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: time.Second})
	if err == nil || verdict != QualityWait {
		t.Fatalf("usage-only stream must surface empty-stream error, verdict=%s err=%v", verdict, err)
	}
	_ = replay.Close()
}

// TestOversizedLineFailsOpen：>1MiB 的单行无法可靠解析，按与 4MiB 缓冲上限一致的
// fail-open 放行（丢弃缓冲会丢掉行内推理证据、误伤健康流）。
func TestOversizedLineFailsOpen(t *testing.T) {
	t.Parallel()
	huge := "data: " + strings.Repeat("x", 1<<21) + "\n\n"
	replay, verdict, _, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(huge)), qualityProtocolChat,
		QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: time.Second})
	if replay != nil {
		defer replay.Close()
	}
	if verdict == QualityDeliver && err == nil {
		t.Fatalf("verdict = %s, oversized garbage must not fail-open", verdict)
	}
}

// TestResponsesMarkerAloneWithholds / TestAnthropicMarkerAloneWithholds：
// responses 的 reasoning item 头与 anthropic 的 thinking 块头单独出现（无文本增量）都是 B 形态，必须扣留。
func TestResponsesMarkerAloneWithholds(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&state, []byte(strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"answer without any reasoning text"}`,
		`data: {"type":"response.completed","response":{"usage":{"output_tokens":64,"output_tokens_details":{"reasoning_tokens":60}}}}`,
		"",
	}, "\n")))
	if ClassifyQualityHold(state.signals(), 32) != QualityWithhold {
		t.Fatalf("responses item header alone must withhold: %#v", state.signals())
	}
}

func TestAnthropicMarkerAloneWithholds(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolAnthropic}
	ObserveQualityChunk(&state, []byte(strings.Join([]string{
		`data: {"type":"content_block_start","content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"answer without thinking deltas"}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")))
	if ClassifyQualityHold(state.signals(), 32) != QualityWithhold {
		t.Fatalf("anthropic thinking block header alone must withhold: %#v", state.signals())
	}
}
