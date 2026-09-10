package egress

import (
	"context"
	"io"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// PhysicalCallAdmissionError identifies a local execution gate failure. It must
// not enter upstream health or connection retry classification.
type PhysicalCallAdmissionError = neterror.LocalExecutionError

func IsPhysicalCallAdmissionError(err error) bool { return neterror.IsLocalExecution(err) }

// MarkPhysicalExecutionError separates an expired owner deadline from a
// transport's shorter timeout. Only an explicitly configured long execution
// supplies this deadline; ordinary HTTP request behavior remains unchanged.
func MarkPhysicalExecutionError(ctx context.Context, err error) error {
	if err == nil || err == io.EOF || neterror.IsLocalExecution(err) {
		return err
	}
	value := physicalCallFromContext(ctx)
	if value.trace != nil && !value.trace.ledger.deadline.IsZero() && !time.Now().Before(value.trace.ledger.deadline) {
		return &neterror.LocalExecutionError{Err: err}
	}
	return err
}

// WithPhysicalCallBatches opts a long execution into acknowledged-entry pruning.
// before runs once per fresh submission, outside the fact lock and serialized
// with other submissions. The execution owner flushes receipts and reserves its
// durable permit there. The unacknowledged fact ceiling remains 128.
func WithPhysicalCallBatches(ctx context.Context, deadline time.Time, before func(context.Context) error) context.Context {
	value := physicalCallFromContext(ctx)
	if value.trace == nil || before == nil {
		return ctx
	}
	ledger := &value.trace.ledger
	ledger.beginMu.Lock()
	defer ledger.beginMu.Unlock()
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	// A request cannot replace an existing owner or change ledger mode after I/O.
	if ledger.before != nil || ledger.started != 0 {
		return ctx
	}
	value.trace.ledger.pruneConfirmed = true
	value.trace.ledger.deadline = deadline
	value.trace.ledger.before = before
	return ctx
}

// PhysicalCallCount includes acknowledged entries pruned by a long execution.
func PhysicalCallCount(ctx context.Context) uint64 {
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return 0
	}
	ledger := &value.trace.ledger
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return ledger.started
}
