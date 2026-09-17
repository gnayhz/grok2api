package physical

import (
	"context"
	"time"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// PhysicalCallAdmissionError identifies a local execution gate failure. It must
// not enter upstream health or connection retry classification.
type PhysicalCallAdmissionError = neterror.LocalExecutionError

// MaxPhysicalCalls bounds unacknowledged facts per logical execution.
const MaxPhysicalCalls = 128

// ErrPhysicalCallLimit reports a saturated physical-call ledger.
var ErrPhysicalCallLimit = inferencedomain.ErrAttemptBudget

func IsPhysicalCallAdmissionError(err error) bool { return neterror.IsLocalExecution(err) }

// MarkPhysicalExecutionError separates an expired owner deadline from a
// transport's shorter timeout through the execution-owned journal.
func MarkPhysicalExecutionError(ctx context.Context, err error) error {
	if journal := JournalFromContext(ctx); journal != nil {
		return journal.MarkExecutionError(ctx, err)
	}
	return err
}

// WithPhysicalCallBatches opts a long execution into acknowledged-entry
// pruning through its journal.
func WithPhysicalCallBatches(ctx context.Context, deadline time.Time, before func(context.Context) error) context.Context {
	if journal := JournalFromContext(ctx); journal != nil && before != nil {
		journal.ConfigureBatches(deadline, before)
	}
	return ctx
}

// PhysicalCallCount includes acknowledged entries pruned by a long execution.
func PhysicalCallCount(ctx context.Context) uint64 {
	if journal := JournalFromContext(ctx); journal != nil {
		return journal.Count()
	}
	return 0
}

// BeginPhysicalCall reserves a bounded ledger entry before I/O through the
// execution-owned journal.
func BeginPhysicalCall(ctx context.Context) error {
	if journal := JournalFromContext(ctx); journal != nil {
		return journal.Begin(ctx)
	}
	return AcquirePhysicalCallBudget(ctx)
}

// RecordPhysicalCall records one transport exchange (header outcome and
// metric). Body wrapping is performed by the network adapter.
func RecordPhysicalCall(ctx context.Context, status int, err error) {
	if journal := JournalFromContext(ctx); journal != nil {
		journal.RecordExchange(ctx, status, err)
		journal.RecordMetric(ctx, status, err)
	}
}

// ObservePhysicalUsage is a fallback for adapters without a canonical payload
// observer. Client protocol conversion can omit counters and must not
// overwrite usage already observed on the physical response.
func ObservePhysicalUsage(ctx context.Context, id string, usage jsonpeek.TokenUsage) {
	if journal := JournalFromContext(ctx); journal != nil {
		journal.ObserveUsage(ctx, id, usage, false)
	}
}

// ObservePhysicalPayload receives the decompressed canonical payload and
// forwards root response usage to the journal. It only accepts root usage,
// never a nested user/tool field with that name.
func ObservePhysicalPayload(ctx context.Context, id string, data []byte) {
	if !jsonpeek.Valid(data) {
		return
	}
	var response, usage []byte
	jsonpeek.ObjectFields(data, func(key, value []byte) bool {
		switch string(key) {
		case "response":
			response = value
		case "usage":
			usage = value
		}
		return true
	})
	if len(response) > 0 {
		jsonpeek.ObjectFields(response, func(key, value []byte) bool {
			if string(key) == "usage" {
				usage = value
			}
			return true
		})
	}
	if len(usage) == 0 {
		return
	}
	if journal := JournalFromContext(ctx); journal != nil {
		journal.ObserveUsage(ctx, id, jsonpeek.TokenUsageObject(usage), true)
	}
}

// ObserveCanonicalPhysicalUsage accepts counters computed by the adapter from
// its native protocol. Client conversion cannot replace this observation.
func ObserveCanonicalPhysicalUsage(ctx context.Context, id string, usage jsonpeek.TokenUsage) {
	if journal := JournalFromContext(ctx); journal != nil {
		journal.ObserveUsage(ctx, id, usage, true)
	}
}

// ObservePhysicalGeneration stores the adapter's protocol observation without
// interpreting it as admission, delivery or a billing decision.
func ObservePhysicalGeneration(ctx context.Context, id, outcome string) {
	if journal := JournalFromContext(ctx); journal != nil {
		journal.ObserveGeneration(ctx, id, outcome)
	}
}

// PhysicalFacts excludes an optional response that is still being handed to
// the client; fallback usage is only available when delivery finalizes.
func PhysicalFacts(ctx context.Context, pendingID ...string) []attemptmeta.PhysicalFact {
	if journal := JournalFromContext(ctx); journal != nil {
		return journal.Facts(ctx, pendingID...)
	}
	return nil
}

// ConfirmPhysicalFacts marks facts persisted by their owning receipt writer.
func ConfirmPhysicalFacts(ctx context.Context, facts []attemptmeta.PhysicalFact) {
	if journal := JournalFromContext(ctx); journal != nil {
		journal.Confirm(ctx, facts)
	}
}

// PhysicalObservations retains request facts independently of pending receipt
// writes. Acknowledging a receipt does not erase upstream usage.
func PhysicalObservations(ctx context.Context) []attemptmeta.PhysicalFact {
	if journal := JournalFromContext(ctx); journal != nil {
		return journal.Observations(ctx)
	}
	return nil
}
