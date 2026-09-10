package gateway

import (
	"context"
	"fmt"
	"time"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

type qualityEventRecorder struct{ value QualityEventRecorder }

const maxLocalQualityOwners = 8192

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
	facts := infraegress.PhysicalFacts(ctx, pendingID...)
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
	infraegress.ConfirmPhysicalFacts(ctx, facts)
	return nil
}

// finishPhysicalReceipt snapshots only finalized exchanges. Callers close/join
// their producers first; an absent fact must never be reported as persisted.
func (s *Service) finishPhysicalReceipt(ctx context.Context) string {
	outcome := "not_required"
	if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
		if _, ok := recorder.value.(PhysicalEventRecorder); ok {
			outcome = "not_recorded"
			if infraegress.PhysicalCallCount(ctx) > 0 {
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
			s.selector.holdLocalQuality(obs.AccountID, obs.Attempt.ID, obs.At.Add(ttl))
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
	if observer := s.qualityObservationObserver(); observer != nil {
		observer.RecordQualityObservation(obs)
	}
	return nil
}

// The embedded gateway and a failed persistence attempt retain a short local
// hold without changing manual Enabled or account health. Production admission
// still requires its authoritative database check; this is only a local fence.
func (s *Selector) holdLocalQuality(accountID uint64, owner string, until time.Time) {
	s.qualityHoldsMu.Lock()
	defer s.qualityHoldsMu.Unlock()
	now := time.Now()
	ownerCount := 0
	for account, owners := range s.qualityHolds {
		for key, expiry := range owners {
			if !expiry.After(now) {
				delete(owners, key)
			}
		}
		if len(owners) == 0 {
			delete(s.qualityHolds, account)
		}
		ownerCount += len(owners)
	}
	if s.qualityHolds == nil {
		s.qualityHolds = make(map[uint64]map[string]time.Time)
	}
	if _, exists := s.qualityHolds[accountID][owner]; exists {
		return
	}
	if ownerCount >= maxLocalQualityOwners {
		// Retaining another owner would exceed local protection capacity. A
		// short global fence preserves rejection without evicting active owners.
		if until.After(s.qualityHoldOverflow) {
			s.qualityHoldOverflow = until
		}
		s.qualityHoldOverflows++
		return
	}
	if s.qualityHolds[accountID] == nil {
		s.qualityHolds[accountID] = make(map[string]time.Time)
	}
	if _, exists := s.qualityHolds[accountID][owner]; !exists {
		s.qualityHolds[accountID][owner] = until
	}
}

func (s *Selector) localQualityAllowed(accountID uint64, now time.Time) bool {
	s.qualityHoldsMu.Lock()
	defer s.qualityHoldsMu.Unlock()
	if s.qualityHoldOverflow.After(now) {
		return false
	}
	for _, until := range s.qualityHolds[accountID] {
		if until.After(now) {
			return false
		}
	}
	delete(s.qualityHolds, accountID)
	return true
}

func physicalUsage(usage Usage) jsonpeek.TokenUsage {
	return jsonpeek.TokenUsage{Found: usage.Reported, Input: usage.InputTokens, Output: usage.OutputTokens, Total: usage.TotalTokens, Reasoning: usage.ReasoningTokens, Cached: usage.CachedInputTokens,
		CostTicks: usage.CostInUSDTicks, Sources: usage.NumSourcesUsed, ServerTools: usage.NumServerSideToolsUsed, ContextInput: usage.ContextInputTokens, ContextOutput: usage.ContextOutputTokens}
}
