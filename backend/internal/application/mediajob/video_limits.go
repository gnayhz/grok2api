package mediajob

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
)

const VideoPhysicalCallLimit = 8192

type PhysicalBudgetStore interface {
	StartMediaJobExecutionLimits(ctx context.Context, jobID, claimToken string, limits media.ExecutionLimits) error
	ReserveMediaJobPhysicalCall(ctx context.Context, jobID, claimToken string, at time.Time) error
	ConfirmMediaJobPhysicalCalls(ctx context.Context, jobID, claimToken string, previous, confirmed uint32) error
}

type PhysicalEventSink interface {
	RecordPhysicalEvents(context.Context, []attemptmeta.PhysicalFact) error
}

type Limits struct {
	Store    PhysicalBudgetStore
	Sink     func() PhysicalEventSink
	LogError func(jobID string, err error)
	Journals portphysical.JournalFactory
}

type limitsKey struct{}

type executionBudget struct {
	mu              sync.Mutex
	store           PhysicalBudgetStore
	sink            func() PhysicalEventSink
	id, claim       string
	limits          media.ExecutionLimits
	err             error
	permitUncertain bool
}

func (l *Limits) Start(ctx context.Context, job *media.Job, jobTimeout time.Duration) (context.Context, error) {
	if err := job.Limits.Validate(); err != nil {
		return ctx, err
	}
	if job.Limits.Version == 0 {
		deadline := time.Now().UTC().Add(jobTimeout).Truncate(time.Microsecond)
		limits := media.ExecutionLimits{Version: 1, Deadline: &deadline, PhysicalLimit: VideoPhysicalCallLimit}
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := l.Store.StartMediaJobExecutionLimits(writeCtx, job.ID, job.ClaimToken, limits)
		cancel()
		if err != nil {
			return ctx, err
		}
		job.Limits = limits
	}
	budget := &executionBudget{store: l.Store, sink: l.Sink, id: job.ID, claim: job.ClaimToken, limits: job.Limits}
	ctx = attemptmeta.WithRequest(ctx, job.ID+"_"+job.ClaimToken, 0, "", nil)
	ctx = portphysical.WithPhysicalCallTrace(ctx, l.Journals.NewPhysicalJournal(), job.Provider, "video")
	ctx = context.WithValue(ctx, limitsKey{}, budget)
	return portphysical.WithPhysicalCallBatches(ctx, *job.Limits.Deadline, budget.beforeCall), nil
}

func (b *executionBudget) beforeCall(ctx context.Context) error {
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
	if len(portphysical.PhysicalObservations(ctx)) >= portphysical.MaxPhysicalCalls {
		return portphysical.ErrPhysicalCallLimit
	}
	if !b.limits.Deadline.After(time.Now()) {
		return context.DeadlineExceeded
	}
	if b.limits.Reserved >= b.limits.PhysicalLimit {
		return media.ErrPhysicalBudgetExhausted
	}
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := b.store.ReserveMediaJobPhysicalCall(writeCtx, b.id, b.claim, time.Now().UTC()); err != nil {
		b.err = err
		b.permitUncertain = true
		return err
	}
	b.limits.Reserved++
	return nil
}

func (b *executionBudget) flushLocked(ctx context.Context) error {
	facts := portphysical.PhysicalFacts(ctx)
	if len(facts) == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	var sink PhysicalEventSink
	if b.sink != nil {
		sink = b.sink()
	}
	if sink == nil {
		portphysical.ConfirmPhysicalFacts(ctx, facts)
		return nil
	}
	if err := sink.RecordPhysicalEvents(writeCtx, facts); err != nil {
		return err
	}
	confirmed := b.limits.Confirmed + uint32(len(facts))
	if err := b.store.ConfirmMediaJobPhysicalCalls(writeCtx, b.id, b.claim, b.limits.Confirmed, confirmed); err != nil {
		return err
	}
	b.limits.Confirmed = confirmed
	portphysical.ConfirmPhysicalFacts(ctx, facts)
	return nil
}

func (l *Limits) FinishReceipt(ctx context.Context, job media.Job) string {
	limits := job.Limits
	if budget, ok := ctx.Value(limitsKey{}).(*executionBudget); ok {
		budget.mu.Lock()
		err := budget.flushLocked(ctx)
		limits = budget.limits
		uncertain := budget.permitUncertain
		budget.mu.Unlock()
		if err != nil {
			if l.LogError != nil {
				l.LogError(job.ID, err)
			}
			return "failed"
		}
		if uncertain {
			return "unconfirmed"
		}
	}
	if l.Sink != nil {
		if sink := l.Sink(); sink != nil {
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

func LocalExecutionFailure(ctx context.Context, err error) (string, bool) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "execution_deadline", true
	}
	if errors.Is(err, media.ErrPhysicalBudgetExhausted) || errors.Is(err, portphysical.ErrPhysicalCallLimit) {
		return "physical_budget_exhausted", true
	}
	if portphysical.IsPhysicalCallAdmissionError(err) || errors.Is(err, netbudget.ErrCapacity) || errors.Is(err, netbudget.ErrClosed) || errors.Is(err, portphysical.ErrClientRetired) {
		return "execution_unavailable", true
	}
	return "", false
}
