package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// Degraded (risk-routed) streams reason server-side — usage reports large
// reasoning token counts — yet never emit reasoning events, and the whole
// burst often coalesces into a single TCP chunk including the trailing usage
// event. The usage claim must never flip the verdict to deliver: these
// regression fixtures mirror streams observed in production.

// bFormWithMarkerChat mirrors the REAL production B-form shape: the converter
// emits the reasoning-start comment as soon as the upstream opens the reasoning
// item (response.output_item.added), but the degraded stream never sends any
// reasoning text — only the content burst and the usage claim follow. The
// marker must not flip hasThinking.
func bFormWithMarkerChat() string {
	return `
: grok2api-reasoning-start

data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}]}

data: {"choices":[{"delta":{"content":"**"},"finish_reason":null,"index":0}]}

data: {"choices":[{"delta":{"content":"Plate tectonics unifies continental drift and seafloor spreading into one framework for Earth dynamics."},"finish_reason":null,"index":0}]}

data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"completion_tokens":420,"completion_tokens_details":{"reasoning_tokens":3308}}}

data: [DONE]

`
}

func TestMarkerAloneStillWithholds(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, MaxAttempts: 3, MinOutputTokens: 32, HoldTimeout: 500 * time.Millisecond, OnExhausted: qualityRetryFailClosed}
	replay, verdict, _, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(bFormWithMarkerChat())), qualityProtocolChat, cfg)
	if err != nil {
		t.Fatalf("peek error: %v", err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("verdict = %s, want withhold: marker without reasoning deltas is B-form", verdict)
	}
}

func bFormBurstChat() string {
	// One coalesced chunk: many content events + usage claiming 928 reasoning
	// tokens, exactly like the live-captured degraded grok-4.5 stream.
	lines := []string{": connected"}
	for _, piece := range strings.Split("To solve 17 * 23, first multiply 17 by 20 to get 340, then 17 by 3 to get 51, and add them for 391", " ") {
		lines = append(lines, "data: {\"choices\":[{\"delta\":{\"content\":\""+piece+" \"},\"finish_reason\":null,\"index\":0}],\"model\":\"grok-4.5\"}")
	}
	lines = append(lines,
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"usage\":{\"completion_tokens\":989,\"completion_tokens_details\":{\"reasoning_tokens\":928}}}",
		"data: [DONE]",
	)
	return strings.Join(lines, "\n") + "\n\n"
}

func TestDegradedUsageClaimNeverDeliversChat(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolChat}
	ObserveQualityChunk(&state, []byte(bFormBurstChat()))
	sig := state.signals()
	if sig.HasThinking {
		t.Fatalf("usage reasoning claim must not count as thinking evidence: %#v", sig)
	}
	if sig.ReasoningTokens != 928 {
		t.Fatalf("audit field lost: %#v", sig)
	}
	if verdict := ClassifyQualityHold(sig, 32); verdict != QualityWithhold {
		t.Fatalf("degraded burst must withhold, got %s (%#v)", verdict, sig)
	}
}

func TestDegradedUsageClaimNeverDeliversResponses(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&state, []byte(strings.Join([]string{
		"event: response.output_text.delta",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"To solve this, multiply and add the partial products carefully.\"}",
		"",
		"event: response.completed",
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"output_tokens\":989,\"output_tokens_details\":{\"reasoning_tokens\":928}}}}",
		"",
	}, "\n")))
	sig := state.signals()
	if sig.HasThinking {
		t.Fatalf("responses usage claim must not count as thinking: %#v", sig)
	}
	if verdict := ClassifyQualityHold(sig, 32); verdict != QualityWithhold {
		t.Fatalf("degraded responses burst must withhold, got %s", verdict)
	}
}

// The full peek path on a coalesced single-read body: verdict must be
// withhold, not deliver, even though usage arrives in the same read.
func TestPeekQualityStreamWithholdsDegradedBurst(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, MaxAttempts: 3, MinOutputTokens: 32, HoldTimeout: 500 * time.Millisecond, OnExhausted: qualityRetryFailClosed}
	replay, verdict, usage, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(bFormBurstChat())), qualityProtocolChat, cfg)
	if err != nil {
		t.Fatalf("peek error: %v", err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("verdict = %s, want withhold", verdict)
	}
	if usage.ReasoningTokens != 928 {
		t.Fatalf("usage reasoning tokens = %d, want 928 preserved for audit", usage.ReasoningTokens)
	}
}

// A-form sanity: the SSE reasoning-start marker plus a reasoning delta still
// delivers immediately, including when coalesced with usage.
func TestThinkingMarkerStillDeliversWhenCoalesced(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolChat}
	ObserveQualityChunk(&state, []byte(strings.Join([]string{
		": grok2api-reasoning-start",
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan the answer\"}}]}",
		"data: {\"choices\":[{\"delta\":{\"content\":\"the answer is 391\"}}]}",
		"data: {\"usage\":{\"completion_tokens\":40,\"completion_tokens_details\":{\"reasoning_tokens\":20}}}",
		"data: [DONE]",
		"",
	}, "\n")))
	sig := state.signals()
	if !sig.HasThinking {
		t.Fatalf("reasoning events must still deliver: %#v", sig)
	}
	if ClassifyQualityHold(sig, 32) != QualityDeliver {
		t.Fatalf("thinking fixture withheld")
	}
}
