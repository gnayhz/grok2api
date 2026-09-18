package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestdiag"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type qualityEventRecorder struct{ value QualityEventRecorder }

func (s *Service) checkQualityEventCapacity(ctx context.Context) error {
	defer requestdiag.Stage(ctx, "quality_capacity", time.Now())
	if recorder := s.qualityEvents.Load(); recorder != nil {
		if source, ok := recorder.value.(interface{ CheckQualityEventCapacity(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			err := source.CheckQualityEventCapacity(ctx)
			recordQualityDiagnostic(ctx, "capacity", err)
			return err
		}
	}
	return nil
}

func (s *Service) recordPhysicalEvents(ctx context.Context, pendingID ...string) error {
	defer requestdiag.Stage(ctx, "physical_receipt", time.Now())
	facts := portphysical.PhysicalFacts(ctx, pendingID...)
	if len(facts) == 0 {
		return nil
	}
	if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
		if sink, ok := recorder.value.(PhysicalEventRecorder); ok {
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			if err := sink.RecordPhysicalEvents(writeCtx, facts); err != nil {
				recordQualityDiagnostic(ctx, "physical_receipt", err)
				return err
			}
		}
	}
	portphysical.ConfirmPhysicalFacts(ctx, facts)
	return nil
}

// finishPhysicalReceipt snapshots only finalized exchanges. Callers close/join
// their producers first; an absent fact must never be reported as persisted.
func (s *Service) finishPhysicalReceipt(ctx context.Context) string {
	outcome := "not_required"
	if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
		if _, ok := recorder.value.(PhysicalEventRecorder); ok {
			outcome = "not_recorded"
			if portphysical.PhysicalCallCount(ctx) > 0 {
				outcome = "committed"
			}
		}
	}
	if err := s.recordPhysicalEvents(ctx); err != nil {
		s.logger.Error("physical_attempt_events_failed", "error", err)
		return "failed"
	}
	return outcome
}

func (s *Service) SetQualityEventRecorder(recorder QualityEventRecorder) {
	s.qualityEvents.Store(&qualityEventRecorder{value: recorder})
}

func (s *Service) recordQualityEvent(ctx context.Context, obs QualityObservation, ttl time.Duration) error {
	defer requestdiag.Stage(ctx, "quality_receipt", time.Now())
	localHold := func() {
		if obs.Outcome == QualityObservedDegraded && s.selector != nil {
			s.selector.HoldLocalQuality(obs.AccountID, obs.Attempt.ID, obs.At.Add(ttl))
		}
	}
	if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if err := recorder.value.RecordQualityEvent(writeCtx, obs, ttl); err != nil {
			recordQualityDiagnostic(ctx, "event_receipt", err)
			localHold()
			return fmt.Errorf("persist guard event: %w", err)
		}
	} else {
		localHold()
	}
	return nil
}

func recordQualityDiagnostic(ctx context.Context, stage string, err error) {
	if err == nil {
		return
	}
	reason := "store_error"
	var classified interface{ StoreDiagnosticReason() string }
	if errors.As(err, &classified) {
		reason = classified.StoreDiagnosticReason()
	}
	if kind, ok := repository.StoreFaultKindOf(err); ok {
		reason = "store_" + string(kind)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		reason = "canceled"
	}
	requestdiag.Failure(ctx, "quality", stage, reason)
}

func physicalUsage(usage Usage) jsonpeek.TokenUsage {
	return jsonpeek.TokenUsage{Found: usage.Reported, Input: usage.InputTokens, Output: usage.OutputTokens, Total: usage.TotalTokens, Reasoning: usage.ReasoningTokens, Cached: usage.CachedInputTokens, CachedReported: usage.CachedInputTokensReported,
		CostTicks: usage.CostInUSDTicks, Sources: usage.NumSourcesUsed, ServerTools: usage.NumServerSideToolsUsed, ContextInput: usage.ContextInputTokens, ContextOutput: usage.ContextOutputTokens}
}
