package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

// Use the real default durations on a virtual clock. Both stream owners must
// distinguish waiting for data from waiting for evidence after data arrives.
func TestQualityLivenessPhases(t *testing.T) {
	const created = "data: {\"type\":\"response.created\"}\n\n"
	const thinking = "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"plan\"}\n\n"
	const emptyDelta = "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"\"}\n\n"
	type frame struct {
		delay time.Duration
		data  string
	}
	for _, canonical := range []bool{false, true} {
		name := "bytes"
		if canonical {
			name = "canonical"
		}
		for _, tc := range []struct {
			name     string
			frames   []frame
			deadline time.Duration
			cancelAt time.Duration
			elapsed  time.Duration
			verdict  QualityVerdict
			err      error
		}{
			{name: "silent", elapsed: 5 * time.Second, verdict: QualityWait, err: errQualityCreatedTimeout},
			{name: "keepalive", frames: []frame{{time.Second, ": keepalive\n\n"}, {3 * time.Second, ": keepalive\n\n"}}, elapsed: 5 * time.Second, verdict: QualityWait, err: errQualityCreatedTimeout},
			{name: "partial_data", frames: []frame{{time.Second, "data: {\"type\":"}}, elapsed: 5 * time.Second, verdict: QualityWait, err: errQualityCreatedTimeout},
			{name: "expired_first_event", frames: []frame{{5 * time.Second, thinking}}, elapsed: 5 * time.Second, verdict: QualityWait, err: errQualityCreatedTimeout},
			{name: "queued_healthy", frames: []frame{{4 * time.Second, created}, {2 * time.Second, thinking}}, elapsed: 6 * time.Second, verdict: QualityDeliver},
			{name: "evidence_silence", frames: []frame{{4 * time.Second, created}}, elapsed: 7500 * time.Millisecond, verdict: QualityWait, err: errQualityEvidenceTimeout},
			{name: "metadata_does_not_extend", frames: []frame{{time.Second, created}, {2 * time.Second, created}, {time.Second, emptyDelta}}, elapsed: 4500 * time.Millisecond, verdict: QualityWait, err: errQualityEvidenceTimeout},
			{name: "immediate_thinking", frames: []frame{{0, thinking}}, verdict: QualityDeliver},
			{name: "immediate_withhold", frames: []frame{{0, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}\n\n"}}, verdict: QualityWithhold},
			{name: "total_deadline", frames: []frame{{4 * time.Second, created}}, deadline: 6 * time.Second, elapsed: 6 * time.Second, verdict: QualityWait, err: context.DeadlineExceeded},
			{name: "cancel_before_data", cancelAt: time.Second, elapsed: time.Second, verdict: QualityWait, err: context.Canceled},
			{name: "cancel_after_data", frames: []frame{{4 * time.Second, created}}, cancelAt: 4500 * time.Millisecond, elapsed: 4500 * time.Millisecond, verdict: QualityWait, err: context.Canceled},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx := context.Background()
					if tc.deadline > 0 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, tc.deadline)
						defer cancel()
					}
					if tc.cancelAt > 0 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						defer cancel()
						timer := time.AfterFunc(tc.cancelAt, cancel)
						defer timer.Stop()
					}
					pool := responsebuffer.NewPool(2 << 20)
					budget := pool.Request(1 << 20)
					ctx = responsebuffer.WithContext(ctx, budget)
					reader, writer := io.Pipe()
					stop, done := make(chan struct{}), make(chan struct{})
					go func() {
						defer close(done)
						for _, f := range tc.frames {
							if f.delay > 0 {
								timer := time.NewTimer(f.delay)
								select {
								case <-timer.C:
								case <-stop:
									timer.Stop()
									return
								}
							}
							if _, err := io.WriteString(writer, f.data); err != nil {
								return
							}
						}
						<-stop // A healthy decision must not wait for EOF.
					}()
					var body io.ReadCloser = reader
					if canonical {
						body = responseflow.New(reader, budget)
					}
					defer func() {
						close(stop)
						_ = body.Close()
						_ = writer.Close()
						<-done
						if used := pool.Snapshot().Used; used != 0 {
							t.Errorf("response buffers retained: %d", used)
						}
					}()
					started := time.Now()
					replay, verdict, _, _, err := peekQualityStreamReport(ctx, body, qualityProtocolResponses, QualityRetryRuntime{})
					defer replay.Close()
					if !errors.Is(err, tc.err) || verdict != tc.verdict || time.Since(started) != tc.elapsed {
						t.Fatalf("verdict=%s err=%v elapsed=%s; want %s, %v, %s", verdict, err, time.Since(started), tc.verdict, tc.err, tc.elapsed)
					}
					if verdict == QualityDeliver {
						var want strings.Builder
						for _, f := range tc.frames {
							want.WriteString(f.data)
						}
						got := make([]byte, want.Len())
						if _, err := io.ReadFull(replay, got); err != nil || string(got) != want.String() {
							t.Fatalf("admitted prefix changed: %q, %v", got, err)
						}
					}
					_ = replay.Close()
				})
			})
		}
	}
}

// The client-body parse is only a pre-normalization fallback: shapes that
// become tools or heavy effort only after adapter normalization (Chat
// web_search_options, Messages thinking, effort=max) keep the configured
// phases until the normalized profile replaces them.
func TestLivenessSchedulePrefersNormalizedProfile(t *testing.T) {
	t.Parallel()
	base := QualityRetryRuntime{Enabled: true, AdmissionTimeout: 30 * time.Second,
		CreatedTimeout: 5 * time.Second, EvidenceTimeout: 3500 * time.Millisecond, ToolAdmissionTimeout: 3 * time.Minute}
	for _, body := range []string{
		`{"web_search_options":{}}`,
		`{"thinking":{"type":"enabled","budget_tokens":1500}}`,
		`{"reasoning":{"effort":"max"}}`,
	} {
		if got := qualityLivenessSchedule([]byte(body), "", base); got.CreatedTimeout != base.CreatedTimeout || got.EvidenceTimeout != base.EvidenceTimeout || got.AdmissionTimeout != base.AdmissionTimeout {
			t.Fatalf("pre-normalization body %s changed the schedule: %+v", body, got)
		}
	}
	tools := qualityLivenessScheduleForProfile(true, "", base)
	if tools.AdmissionTimeout != base.ToolAdmissionTimeout || tools.CreatedTimeout != qualitySearchSilenceBudget || tools.EvidenceTimeout != qualitySearchSilenceBudget {
		t.Fatalf("normalized tool profile: %+v", tools)
	}
	// The adapter publishes resolved efforts: aliases like max arrive here as
	// high/xhigh, so the heavy phases apply from the normalized profile.
	for _, effort := range []string{"high", "xhigh", " XHigh "} {
		heavy := qualityLivenessScheduleForProfile(false, effort, base)
		if heavy.CreatedTimeout != qualityHeavyReasoningCreatedBudget || heavy.EvidenceTimeout != qualityHeavyReasoningCreatedBudget || heavy.AdmissionTimeout != base.AdmissionTimeout {
			t.Fatalf("normalized heavy effort %q: %+v", effort, heavy)
		}
	}
}
