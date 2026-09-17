// Package execution owns the per-request physical accounting journal: the
// single mutable ledger of transport submissions, body outcomes and usage
// facts for one logical execution. It implements the port/physical.Journal
// contract; the composition root injects the factory into the owners that
// start logical executions.
package execution

import (
	"context"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

// factory builds one journal per logical execution.
type factory struct{}

// NewPhysicalJournalFactory returns the execution-owned journal factory.
func NewPhysicalJournalFactory() physical.JournalFactory { return factory{} }

func (factory) NewPhysicalJournal() physical.Journal { return &journal{} }

var _ physical.Journal = (*journal)(nil)

type journal struct {
	beginMu        sync.Mutex
	deadline       time.Time
	before         func(context.Context) error
	pruneConfirmed bool
	started        uint64
	mu             sync.Mutex
	calls          map[string]*entry
	order          []string
	ordinal        atomic.Uint64
}

type entry struct {
	fact                 attemptmeta.PhysicalFact
	finalized, persisted bool
	canonicalUsage       bool
}

// Begin reserves a bounded ledger entry before I/O. Every actual transport
// submission has a fresh attempt identity, including connection-only retries.
// A saturated ledger cannot silently drop a billable physical call.
func (j *journal) Begin(ctx context.Context) error {
	meta := physical.MetaFromContext(ctx)
	id := attemptmeta.FromContext(ctx)
	if id.ID == "" {
		return physical.AcquirePhysicalCallBudget(ctx)
	}
	j.beginMu.Lock()
	defer j.beginMu.Unlock()
	j.mu.Lock()
	_, exists := j.calls[id.ID]
	j.mu.Unlock()
	if exists {
		return nil
	}
	if j.before != nil {
		if err := j.before(ctx); err != nil {
			return &physical.PhysicalCallAdmissionError{Err: err, BeforeSubmission: true}
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.calls == nil {
		j.calls = make(map[string]*entry)
	}
	if _, exists := j.calls[id.ID]; exists {
		return nil
	}
	if len(j.calls) >= physical.MaxPhysicalCalls {
		if j.before != nil {
			return &physical.PhysicalCallAdmissionError{Err: physical.ErrPhysicalCallLimit, BeforeSubmission: true}
		}
		return physical.ErrPhysicalCallLimit
	}
	if err := physical.AcquirePhysicalCallBudget(ctx); err != nil {
		if j.before != nil {
			return &physical.PhysicalCallAdmissionError{Err: err, BeforeSubmission: true}
		}
		return err
	}
	j.calls[id.ID] = &entry{fact: attemptmeta.PhysicalFact{Attempt: id, Plane: meta.Plane, Stage: meta.Stage, HeaderOutcome: "pending", BodyOutcome: "pending"}}
	j.order = append(j.order, id.ID)
	j.started++
	return nil
}

// RecordExchange records the header-level outcome of one exchange.
// status < 0 means no response.
func (j *journal) RecordExchange(ctx context.Context, status int, err error) {
	id := attemptmeta.FromContext(ctx)
	if id.ID == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e := j.calls[id.ID]
	if e == nil {
		return
	}
	e.fact.HeaderOutcome = outcome(status, err)
	if status > 0 {
		e.fact.Status = status
	}
	if status <= 0 || err != nil {
		e.fact.BodyOutcome = "unavailable"
		e.fact.At = time.Now().UTC()
		e.fact.DurationMS = time.Since(id.StartedAt).Milliseconds()
		e.finalized = true
	}
}

// RecordMetric emits the per-call transport metric with the request-wide
// ordinal.
func (j *journal) RecordMetric(ctx context.Context, status int, err error) {
	meta := physical.MetaFromContext(ctx)
	ordinal := j.ordinal.Add(1)
	perfmetrics.Default.Inc("upstream_physical_call_total", perfmetrics.Labels{
		Subsystem: "upstream",
		Operation: meta.Operation,
		Provider:  meta.Provider,
		Plane:     meta.Plane,
		Stage:     meta.Stage,
		Ordinal:   ordinalBucket(ordinal),
		Outcome:   outcome(status, err),
	})
}

func (j *journal) ObserveBody(id string, n int, outcome string) {
	if id == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.calls[id]; e != nil && !e.persisted {
		e.fact.BodyBytes += int64(n)
		if outcome != "" && e.fact.BodyOutcome == "pending" {
			e.fact.BodyOutcome = outcome
		}
	}
}

func (j *journal) FinalizeBody(id string, outcome string, startedAt time.Time) {
	if id == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.calls[id]; e != nil && !e.persisted {
		if e.fact.BodyOutcome == "pending" {
			e.fact.BodyOutcome = outcome
		}
		e.fact.At = time.Now().UTC()
		e.fact.DurationMS = time.Since(startedAt).Milliseconds()
		e.finalized = true
	}
}

func (j *journal) MarkUpgraded(id string) {
	if id == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.calls[id]; e != nil {
		e.fact.HeaderOutcome, e.fact.Status = "upgraded", 101
	}
}

func (j *journal) ObserveUsage(ctx context.Context, id string, usage jsonpeek.TokenUsage, canonical bool) {
	if !usage.Found {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.calls[id]; e != nil && !e.persisted && (canonical || !e.canonicalUsage) {
		e.fact.Usage = usage
		e.canonicalUsage = canonical
	}
}

func (j *journal) ObserveGeneration(ctx context.Context, id, outcome string) {
	if outcome != "completed" && outcome != "failed" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.calls[id]; e != nil && !e.persisted {
		e.fact.GenerationOutcome = outcome
	}
}

func (j *journal) Facts(ctx context.Context, pendingID ...string) []attemptmeta.PhysicalFact {
	j.mu.Lock()
	defer j.mu.Unlock()
	facts := make([]attemptmeta.PhysicalFact, 0, len(j.order))
	for _, id := range j.order {
		if len(pendingID) > 0 && id == pendingID[0] {
			continue
		}
		if e := j.calls[id]; e.finalized && !e.persisted {
			facts = append(facts, e.fact)
		}
	}
	return facts
}

func (j *journal) Observations(ctx context.Context) []attemptmeta.PhysicalFact {
	j.mu.Lock()
	defer j.mu.Unlock()
	facts := make([]attemptmeta.PhysicalFact, 0, len(j.order))
	for _, id := range j.order {
		facts = append(facts, j.calls[id].fact)
	}
	return facts
}

func (j *journal) Confirm(ctx context.Context, facts []attemptmeta.PhysicalFact) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, fact := range facts {
		if e := j.calls[fact.Attempt.ID]; e != nil {
			e.persisted = true
			if j.pruneConfirmed {
				delete(j.calls, fact.Attempt.ID)
			}
		}
	}
	if j.pruneConfirmed {
		remaining := j.order[:0]
		for _, id := range j.order {
			if j.calls[id] != nil {
				remaining = append(remaining, id)
			}
		}
		clear(j.order[len(remaining):])
		j.order = remaining
	}
}

// ConfigureBatches opts a long execution into acknowledged-entry pruning.
// before runs once per fresh submission, outside the fact lock and serialized
// with other submissions. A request cannot replace an existing owner or
// change ledger mode after I/O.
func (j *journal) ConfigureBatches(deadline time.Time, before func(context.Context) error) {
	j.beginMu.Lock()
	defer j.beginMu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.before != nil || j.started != 0 {
		return
	}
	j.pruneConfirmed = true
	j.deadline = deadline
	j.before = before
}

// MarkExecutionError separates an expired owner deadline from a transport's
// shorter timeout. Only an explicitly configured long execution supplies this
// deadline; ordinary HTTP request behavior remains unchanged.
func (j *journal) MarkExecutionError(ctx context.Context, err error) error {
	if err == nil || err == io.EOF || neterror.IsLocalExecution(err) {
		return err
	}
	j.mu.Lock()
	deadline := j.deadline
	j.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return &neterror.LocalExecutionError{Err: err}
	}
	return err
}

func (j *journal) Count() uint64 { return j.started }

func ordinalBucket(value uint64) string {
	if value >= 5 {
		return "5_plus"
	}
	return strconv.FormatUint(value, 10)
}

// outcome classifies one exchange for metrics and header facts. status < 0
// means the transport returned no response.
func outcome(status int, err error) string {
	if err != nil {
		return "transport_error"
	}
	if status < 0 {
		return "empty_response"
	}
	switch {
	case status == 101:
		return "upgraded"
	case status >= 200 && status < 300:
		return "success"
	case status >= 300 && status < 400:
		return "redirect"
	case status >= 400 && status < 500:
		return "client_error"
	case status >= 500 && status < 600:
		return "server_error"
	default:
		return "other"
	}
}
