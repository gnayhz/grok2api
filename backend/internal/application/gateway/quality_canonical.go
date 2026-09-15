package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

// The physical assembler owns framing and retained events. Admission observes
// those events once; converters later borrow the exact retained frames.
func peekCanonicalQualityStream(ctx context.Context, body io.ReadCloser, stream *responseflow.Stream, protocol string, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, qualityHoldFingerprint, error) {
	cfg = normalizeQualityRetry(cfg)
	state := qualityScanState{kernel: cfg.kernel, protocol: protocol, startedAt: time.Now()}
	var useful atomic.Bool
	liveness := newQualityLivenessTimer(cfg)
	defer liveness.timer.Stop()
	verdict := QualityWait
	var verdictErr error
	finished := make(chan error, 1)
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				finished <- fmt.Errorf("response admission panic: %v", failure)
			}
		}()
		finished <- stream.Hold(qualityHoldMaxBufferBytes, func(event *responseflow.Event) (bool, error) {
			if !event.HasData {
				return false, nil
			}
			if !state.sawDataEvent {
				if err := liveness.observeData(); err != nil {
					return true, err
				}
				state.sawDataEvent = true
			}
			if bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
				state.terminal = true
			} else {
				observeQualityPayload(&state, event.Data)
			}
			useful.Store(state.hasThinking || state.visibleRunes > 0 || state.aggregateRunes > 0 || state.semanticOutput)
			verdict, verdictErr = state.streamVerdict(cfg.ReasoningExpected)
			return verdict != QualityWait || verdictErr != nil, verdictErr
		})
	}()
	var err error
wait:
	for {
		select {
		case err = <-finished:
			break wait
		case <-ctx.Done():
			err = qualityPeekAbortError(ctx, ctx.Err())
			_ = body.Close()
			<-finished
			break wait
		case <-liveness.timer.C:
			if timeout := liveness.timeout(); timeout != nil && !useful.Load() {
				err = timeout
				_ = body.Close()
				<-finished
				break wait
			}
		}
	}
	if err == io.EOF {
		state.terminal = true
		verdict, verdictErr = state.streamVerdict(cfg.ReasoningExpected)
		err = verdictErr
	}
	if errors.Is(err, responsebuffer.ErrLimit) {
		err = errQualityHoldLimit
	}
	if ctx.Err() != nil {
		err = qualityPeekAbortError(ctx, ctx.Err())
	}
	if err != nil {
		verdict = QualityWait
	}
	if verdict != QualityDeliver || err != nil {
		_ = body.Close()
		body = io.NopCloser(bytes.NewReader(nil))
	}
	return body, verdict, state.usage, state.fingerprint(verdict, err), err
}
