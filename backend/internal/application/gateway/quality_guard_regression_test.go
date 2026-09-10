package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGuardDecisionIndependentOfReadBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct{ protocol, first, late string }{
		{qualityProtocolResponses, `{"type":"response.output_text.delta","delta":"answer first"}`, `{"type":"response.reasoning_text.delta","delta":"too late"}`},
		{qualityProtocolResponses, `{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1"}}`, `{"type":"response.reasoning_text.delta","delta":"too late"}`},
		{qualityProtocolChat, `{"choices":[{"delta":{"content":"answer first"}}]}`, `{"choices":[{"delta":{"reasoning_content":"too late"}}]}`},
		{qualityProtocolAnthropic, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer first"}}`, `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"too late"}}`},
	}
	for _, tc := range cases {
		payload := "data: " + tc.first + "\n\ndata: " + tc.late + "\n\n"
		for _, chunk := range []int{1, 7, len(payload)} {
			replay, verdict, _, fp, err := peekQualityStreamReport(context.Background(), io.NopCloser(&guardChunkReader{data: []byte(payload), size: chunk}), tc.protocol, QualityRetryRuntime{})
			if replay != nil {
				replay.Close()
			}
			if err != nil || verdict != QualityWithhold || fp.HasThinking {
				t.Errorf("protocol=%s first=%s chunk=%d: verdict=%s thinking=%v err=%v", tc.protocol, tc.first, chunk, verdict, fp.HasThinking, err)
			}
		}
	}
}

type guardChunkReader struct {
	data []byte
	size int
}

func (r *guardChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data[:min(len(r.data), r.size)])
	r.data = r.data[n:]
	return n, nil
}

func TestGuardThinkingRequiresProtocolField(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, protocol, payload string }{
		{"nested responses delta", qualityProtocolResponses, `{"metadata":{"delta":"spoof"},"type":"response.reasoning_text.delta","delta":""}`},
		{"chat metadata", qualityProtocolChat, `{"metadata":{"reasoning_content":"spoof"},"choices":[{"delta":{"content":"answer"}}]}`},
		{"wrong anthropic delta", qualityProtocolAnthropic, `{"type":"content_block_delta","delta":{"type":"signature_delta","thinking":"spoof"}}`},
		{"large marker in text", qualityProtocolResponses, `{"type":"response.output_text.delta","delta":"reasoning_text.delta ` + strings.Repeat("x", 70<<10) + `"}`},
		{"large blank thinking", qualityProtocolResponses, `{"type":"response.reasoning_text.delta","delta":"` + strings.Repeat(" ", 70<<10) + `"}`},
		{"control-only thinking", qualityProtocolResponses, `{"type":"response.reasoning_text.delta","delta":"\u0000\u0001"}`},
		{"truncated thinking", qualityProtocolResponses, `{"type":"response.reasoning_text.delta","delta":"spoof","unfinished":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := qualityScanState{protocol: tc.protocol}
			observeQualityChunk(&state, []byte("data: "+tc.payload+"\n\n"))
			if state.hasThinking {
				t.Fatal("non-evidence became thinking")
			}
		})
	}
}

func TestGuardRetryPolicyCannotReleaseWithhold(t *testing.T) {
	for _, action := range []QualityRetryAction{QualityActionDeliver, "deliver_last", "", "invalid"} {
		service := &Service{}
		service.SetQualityRetryPolicy(stubRetryPolicy{action: action})
		got := service.decideQualityCommit(QualityWithhold, 0, 2, true, qualityRetryFailClosed)
		if got.KeepBody || got.Action != QualityActionReject {
			t.Errorf("policy=%q: %+v", action, got)
		}
	}
	service := &Service{}
	service.SetQualityRetryPolicy(stubRetryPolicy{action: QualityActionRetry})
	if got := service.decideQualityCommit(QualityWithhold, 1, 2, true, qualityRetryFailClosed); got.Action != QualityActionReject {
		t.Fatalf("policy escaped attempt budget: %+v", got)
	}
}

func TestGuardRuntimeOwnsModelSnapshot(t *testing.T) {
	service := &Service{}
	cfg := QualityRetryRuntime{GuardedModels: []string{"grok-4.6"}}
	service.UpdateQualityRetry(cfg)
	cfg.GuardedModels[0] = "mutated"
	if got := service.QualityRetryConfig().GuardedModels[0]; got != "grok-4.6" {
		t.Fatalf("runtime aliased caller: %q", got)
	}
}

func TestGuardUnrecognizedBodyIsNotHealthy(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"error":{"message":"failed"}}`} {
		replay, verdict, _, err := peekQualityBody(io.NopCloser(strings.NewReader(body)), QualityRetryRuntime{})
		replay.Close()
		if verdict == QualityDeliver || err == nil {
			t.Errorf("body=%s verdict=%s err=%v", body, verdict, err)
		}
	}
}

func TestGuardReadPumpNoProgress(t *testing.T) {
	pump := newQualityReadPump(io.NopCloser(&guardEmptyReader{}))
	defer pump.Close()
	// A bounded reader eventually supplies an error, making this regression safe
	// to run against the old busy-loop implementation as well.
	_, err := pump.Read(make([]byte, 1))
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("err=%v, want ErrNoProgress", err)
	}
}

type guardEmptyReader struct{ reads int }

func (r *guardEmptyReader) Read([]byte) (int, error) {
	r.reads++
	if r.reads > 1000 {
		return 0, io.EOF
	}
	return 0, nil
}

func TestGuardReplayPreservesCoalescedHealthyStream(t *testing.T) {
	t.Parallel()
	payload := []byte("data: " + `{"type":"response.reasoning_text.delta","delta":"plan"}` + "\n\ndata: " + `{"type":"response.output_text.delta","delta":"answer"}` + "\n\ndata: [DONE]\n\n")
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(bytes.NewReader(payload)), qualityProtocolResponses, QualityRetryRuntime{})
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("verdict=%s err=%v", verdict, err)
	}
	defer replay.Close()
	got, err := io.ReadAll(replay)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("replay changed: %q err=%v", got, err)
	}
}

// A valid escaped slash or UTF-16 pair is visible evidence, not an empty delta.
func TestGuardJSONEscapesAndBodyWhitespace(t *testing.T) {
	for _, delta := range []string{`"a\/b"`, `"\ud83d\ude00"`, `"思考"`} {
		replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":"+delta+"}\n\n")), qualityProtocolResponses, QualityRetryRuntime{})
		replay.Close()
		if err != nil || verdict != QualityDeliver {
			t.Fatalf("delta=%s verdict=%s err=%v", delta, verdict, err)
		}
	}
	for _, body := range []string{
		`{"output":[{"type":"reasoning","summary":[{"text":"\u200b\ufeff"}]},{"type":"message","content":[{"text":"answer"}]}]}`,
		`{"choices":[{"message":{"reasoning_content":"\u200b\ufeff","content":"answer"}}]}`,
		`{"content":[{"type":"thinking","thinking":"\u200b\ufeff"},{"type":"text","text":"answer"}]}`,
	} {
		replay, verdict, _, err := peekQualityBody(io.NopCloser(strings.NewReader(body)), QualityRetryRuntime{})
		replay.Close()
		if err != nil || verdict != QualityWithhold {
			t.Fatalf("invisible body thinking delivered: verdict=%s err=%v", verdict, err)
		}
	}
}

func TestGuardPreservesReadErrorAfterHealthyPrefix(t *testing.T) {
	payload := []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"plan\"}\n\n")
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(&guardDataErrorReader{data: payload}), qualityProtocolResponses, QualityRetryRuntime{})
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("verdict=%s err=%v", verdict, err)
	}
	defer replay.Close()
	got, err := io.ReadAll(replay)
	if !bytes.Equal(got, payload) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("replay=%q err=%v", got, err)
	}
}

type guardDataErrorReader struct{ data []byte }

func (r *guardDataErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, io.ErrUnexpectedEOF
}

func TestGuardBufferOverflowIsInconclusive(t *testing.T) {
	// Neither comment floods nor unfinished frames establish degradation.
	for _, prefix := range []string{": ", "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\""} {
		raw := &guardClosedBody{Reader: strings.NewReader(prefix + strings.Repeat("x", qualityHoldMaxBufferBytes+100))}
		replay, verdict, _, fp, err := peekQualityStreamReport(context.Background(), raw, qualityProtocolResponses, QualityRetryRuntime{})
		defer replay.Close()
		if verdict != QualityWait || !errors.Is(err, errQualityHoldLimit) || fp.Rule != "buffer_limit" || !raw.closed {
			t.Fatalf("verdict=%s err=%v rule=%s closed=%v", verdict, err, fp.Rule, raw.closed)
		}
	}
}

type guardClosedBody struct {
	io.Reader
	closed bool
}

func (b *guardClosedBody) Close() error { b.closed = true; return nil }

func TestGuardUnknownVerdictCannotCommit(t *testing.T) {
	for _, verdict := range []QualityVerdict{QualityWait, "", "future"} {
		if commit := commitQualityHold(verdict, 0, 2, true); commit.KeepBody || commit.Action != QualityActionReject {
			t.Fatalf("verdict=%q commit=%+v", verdict, commit)
		}
	}
}

func TestGuardCancelledContextDoesNotReadOrDeliver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw := &guardCancelledBody{}
	replay, verdict, _, err := peekQualityStream(ctx, raw, qualityProtocolResponses, QualityRetryRuntime{})
	replay.Close()
	if verdict != QualityWait || !errors.Is(err, context.Canceled) || !raw.closed || raw.read {
		t.Fatalf("verdict=%s err=%v read=%v closed=%v", verdict, err, raw.read, raw.closed)
	}
}

type guardCancelledBody struct{ read, closed bool }

func (b *guardCancelledBody) Read([]byte) (int, error) { b.read = true; return 0, io.EOF }
func (b *guardCancelledBody) Close() error             { b.closed = true; return nil }

func TestGuardEscapedItemIdentityClosesReasoning(t *testing.T) {
	payload := "data: " + `{"item":{"type":"reason\u0069ng"},"type":"response.output_item.done"}` + "\n\n"
	replay, verdict, _, fp, err := peekQualityStreamReport(context.Background(), io.NopCloser(strings.NewReader(payload)), qualityProtocolResponses, QualityRetryRuntime{})
	replay.Close()
	if err != nil || verdict != QualityWithhold || fp.Rule != "item_done" {
		t.Fatalf("verdict=%s rule=%s err=%v", verdict, fp.Rule, err)
	}
}

func TestGuardEffectiveExhaustionPolicyIsFailClosed(t *testing.T) {
	for _, legacy := range []string{"fail_open", "unknown", ""} {
		cfg := normalizeQualityRetry(QualityRetryRuntime{OnExhausted: legacy})
		if cfg.OnExhausted != qualityRetryFailClosed {
			t.Fatalf("effective policy=%q", cfg.OnExhausted)
		}
	}
}

func TestGuardChatRepeatedFieldsAgreeWithDecodedEvidence(t *testing.T) {
	cases := []string{
		`{"choices":[{"delta":{"reasoning_content":"discarded","reasoning_content":"","content":"answer"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"discarded","reasoning_\u0063ontent":"","content":"answer"}}]}`,
		`{"choices":[{"delta":{"content":"discarded","content":"x"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"discarded"},"delta":{"reasoning_content":"","content":"answer"}}]}`,
		`{"choices":[{"finish_reason":"stop","finish_reason":"","delta":{"content":"x"}}]}`,
	}
	for _, payload := range cases {
		fast, decoded := qualityScanState{}, qualityScanState{}
		observeQualityChat(&fast, []byte(payload))
		observeQualityChatDecoded(&decoded, []byte(payload))
		if fast.signals() != decoded.signals() || fast.semanticOutput != decoded.semanticOutput {
			t.Errorf("payload=%s: fast=%+v decoded=%+v", payload, fast.signals(), decoded.signals())
		}
	}
}

func TestGuardReleasesBlockedHTTPUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, prefix string
		verdict      QualityVerdict
		wantErr      error
	}{
		{"withhold", `data: {"type":"response.output_item.done","item":{"type":"reasoning"}}`, QualityWithhold, nil},
		{"healthy close", `data: {"type":"response.reasoning_text.delta","delta":"plan"}`, QualityDeliver, nil},
		{"created timeout", ": keepalive", QualityWait, errQualityCreatedTimeout},
		{"evidence timeout", `data: {"type":"response.created"}`, QualityWait, errQualityEvidenceTimeout},
		{"canceled", `data: {"type":"response.created"}`, QualityWait, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disconnected, shutdown := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.prefix+"\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					close(disconnected)
				case <-shutdown:
				}
			}))
			defer server.Close()
			defer close(shutdown)
			client := server.Client()
			client.Timeout = 3 * time.Second
			response, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := QualityRetryRuntime{CreatedTimeout: 2 * time.Second, EvidenceTimeout: 2 * time.Second}
			switch tc.wantErr {
			case errQualityCreatedTimeout:
				cfg.CreatedTimeout = 25 * time.Millisecond
			case errQualityEvidenceTimeout:
				cfg.EvidenceTimeout = 25 * time.Millisecond
			case context.Canceled:
				cancel()
			}
			replay, verdict, _, err := peekQualityStream(ctx, response.Body, qualityProtocolResponses, cfg)
			if replay != nil {
				defer replay.Close()
			}
			if verdict != tc.verdict || !errors.Is(err, tc.wantErr) {
				t.Fatalf("verdict=%s err=%v; want %s %v", verdict, err, tc.verdict, tc.wantErr)
			}
			if verdict == QualityDeliver {
				_ = replay.Close() // A client may abandon an accepted stream immediately.
			}
			select {
			case <-disconnected:
			case <-time.After(time.Second):
				t.Fatal("blocked HTTP upstream remained connected after the attempt ended")
			}
		})
	}
}
