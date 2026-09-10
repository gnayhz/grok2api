package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

// Two hours of two-second polling needs at most 3600 polls. 8192 leaves room
// for preparation and bounded recovery; retries still share this hard ceiling.
// The fixed deadline remains authoritative if individual requests are slow.
const videoPhysicalCallLimit = 8192

type videoLimitsKey struct{}
type videoExecutionBudget struct {
	mu              sync.Mutex
	service         *Service
	id, claim       string
	limits          media.ExecutionLimits
	err             error
	permitUncertain bool
}

func (s *Service) startVideoLimits(ctx context.Context, job *media.Job) (context.Context, error) {
	if err := job.Limits.Validate(); err != nil {
		return ctx, err
	}
	if job.Limits.Version == 0 {
		deadline := time.Now().UTC().Add(videoJobTimeout).Truncate(time.Microsecond)
		limits := media.ExecutionLimits{Version: 1, Deadline: &deadline, PhysicalLimit: videoPhysicalCallLimit}
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := s.mediaJobs.StartMediaJobExecutionLimits(writeCtx, job.ID, job.ClaimToken, limits)
		cancel()
		if err != nil {
			return ctx, err
		}
		job.Limits = limits
	}
	budget := &videoExecutionBudget{service: s, id: job.ID, claim: job.ClaimToken, limits: job.Limits}
	// The claim is unique across restarts; a resumed attempt cannot collide with
	// any earlier physical journal event even though its local ordinal restarts.
	ctx = attemptmeta.WithRequest(ctx, job.ID+"_"+job.ClaimToken, 0, "", nil)
	ctx = infraegress.WithPhysicalCallTrace(ctx, job.Provider, "video")
	ctx = context.WithValue(ctx, videoLimitsKey{}, budget)
	return infraegress.WithPhysicalCallBatches(ctx, *job.Limits.Deadline, budget.beforeCall), nil
}

func (b *videoExecutionBudget) beforeCall(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.flushLocked(ctx); err != nil {
		b.err = err
		return err
	}
	if len(infraegress.PhysicalObservations(ctx)) >= infraegress.MaxPhysicalCalls {
		return infraegress.ErrPhysicalCallLimit
	}
	if !b.limits.Deadline.After(time.Now()) {
		return context.DeadlineExceeded
	}
	if b.limits.Reserved >= b.limits.PhysicalLimit {
		return media.ErrPhysicalBudgetExhausted
	}
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := b.service.mediaJobs.ReserveMediaJobPhysicalCall(writeCtx, b.id, b.claim, time.Now().UTC()); err != nil {
		b.err = err
		b.permitUncertain = true
		return err
	}
	b.limits.Reserved++
	return nil
}

func (b *videoExecutionBudget) flushLocked(ctx context.Context) error {
	facts := infraegress.PhysicalFacts(ctx)
	if len(facts) == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	var sink PhysicalEventRecorder
	if recorder := b.service.qualityEvents.Load(); recorder != nil && recorder.value != nil {
		sink, _ = recorder.value.(PhysicalEventRecorder)
	}
	if sink == nil {
		// Embedded execution has no receipt obligation. Prune its local entries,
		// but never persist a journal confirmation that another instance could trust.
		infraegress.ConfirmPhysicalFacts(ctx, facts)
		return nil
	}
	if err := sink.RecordPhysicalEvents(writeCtx, facts); err != nil {
		return err
	}
	confirmed := b.limits.Confirmed + uint32(len(facts))
	if err := b.service.mediaJobs.ConfirmMediaJobPhysicalCalls(writeCtx, b.id, b.claim, b.limits.Confirmed, confirmed); err != nil {
		return err
	}
	b.limits.Confirmed = confirmed
	infraegress.ConfirmPhysicalFacts(ctx, facts)
	return nil
}

func (s *Service) finishVideoPhysicalReceipt(ctx context.Context, job media.Job) string {
	limits := job.Limits
	if budget, ok := ctx.Value(videoLimitsKey{}).(*videoExecutionBudget); ok {
		budget.mu.Lock()
		err := budget.flushLocked(ctx)
		limits = budget.limits
		uncertain := budget.permitUncertain
		budget.mu.Unlock()
		if err != nil {
			s.logger.Error("video_physical_receipt_failed", "job_id", job.ID, "error", err)
			return "failed"
		}
		if uncertain {
			return "unconfirmed"
		}
	}
	if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
		if _, ok := recorder.value.(PhysicalEventRecorder); ok {
			if limits.Reserved == 0 {
				return "not_recorded"
			}
			if limits.Reserved != limits.Confirmed {
				return "unconfirmed"
			}
			return "committed"
		}
	}
	return "not_required"
}

func videoLocalExecutionFailure(ctx context.Context, err error) (string, bool) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "execution_deadline", true
	}
	if errors.Is(err, media.ErrPhysicalBudgetExhausted) || errors.Is(err, infraegress.ErrPhysicalCallLimit) {
		return "physical_budget_exhausted", true
	}
	if infraegress.IsPhysicalCallAdmissionError(err) || errors.Is(err, netbudget.ErrCapacity) || errors.Is(err, netbudget.ErrClosed) || errors.Is(err, infraegress.ErrClientRetired) {
		return "execution_unavailable", true
	}
	return "", false
}
