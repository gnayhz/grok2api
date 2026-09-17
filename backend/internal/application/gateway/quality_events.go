package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
)

type qualityEventRecorder struct{ value QualityEventRecorder }

func (s *Service) checkQualityEventCapacity(ctx context.Context) error {
	if recorder := s.qualityEvents.Load(); recorder != nil {
		if source, ok := recorder.value.(interface{ CheckQualityEventCapacity(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			return source.CheckQualityEventCapacity(ctx)
		}
	}
	return nil
}

func (s *Service) recordPhysicalEvents(ctx context.Context, pendingID ...string) error {
	facts := portphysical.PhysicalFacts(ctx, pendingID...)
	if len(facts) == 0 {
		return nil
	}
	if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
		if sink, ok := recorder.value.(PhysicalEventRecorder); ok {
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			if err := sink.RecordPhysicalEvents(writeCtx, facts); err != nil {
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
	localHold := func() {
		if obs.Outcome == QualityObservedDegraded && s.selector != nil {
			s.selector.HoldLocalQuality(obs.AccountID, obs.Attempt.ID, obs.At.Add(ttl))
		}
	}
	if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if err := recorder.value.RecordQualityEvent(writeCtx, obs, ttl); err != nil {
			localHold()
			return fmt.Errorf("persist guard event: %w", err)
		}
	} else {
		localHold()
	}
	return nil
}

func physicalUsage(usage Usage) jsonpeek.TokenUsage {
	return jsonpeek.TokenUsage{Found: usage.Reported, Input: usage.InputTokens, Output: usage.OutputTokens, Total: usage.TotalTokens, Reasoning: usage.ReasoningTokens, Cached: usage.CachedInputTokens, CachedReported: usage.CachedInputTokensReported,
		CostTicks: usage.CostInUSDTicks, Sources: usage.NumSourcesUsed, ServerTools: usage.NumServerSideToolsUsed, ContextInput: usage.ContextInputTokens, ContextOutput: usage.ContextOutputTokens}
}
