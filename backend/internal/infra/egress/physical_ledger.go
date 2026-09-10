package egress

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

const MaxPhysicalCalls = 128

var ErrPhysicalCallLimit = inferencedomain.ErrAttemptBudget

type physicalLedger struct {
	beginMu        sync.Mutex
	deadline       time.Time
	before         func(context.Context) error
	pruneConfirmed bool
	started        uint64
	mu             sync.Mutex
	calls          map[string]*physicalEntry
	order          []string
}
type physicalEntry struct {
	fact                 attemptmeta.PhysicalFact
	finalized, persisted bool
	canonicalUsage       bool
}

// beginPhysicalCall reserves a bounded ledger entry before I/O. Every actual
// transport submission has a fresh attempt identity, including connection-only
// retries. A saturated ledger cannot silently drop a billable physical call.
func beginPhysicalCall(ctx context.Context) error {
	value := physicalCallFromContext(ctx)
	id := attemptmeta.FromContext(ctx)
	if value.trace == nil || id.ID == "" {
		return acquirePhysicalCallBudget(ctx)
	}
	ledger := &value.trace.ledger
	ledger.beginMu.Lock()
	defer ledger.beginMu.Unlock()
	ledger.mu.Lock()
	_, exists := ledger.calls[id.ID]
	ledger.mu.Unlock()
	if exists {
		return nil
	}
	if ledger.before != nil {
		if err := ledger.before(ctx); err != nil {
			return &PhysicalCallAdmissionError{Err: err, BeforeSubmission: true}
		}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.calls == nil {
		ledger.calls = make(map[string]*physicalEntry)
	}
	if _, exists := ledger.calls[id.ID]; exists {
		return nil
	}
	if len(ledger.calls) >= MaxPhysicalCalls {
		if ledger.before != nil {
			return &PhysicalCallAdmissionError{Err: ErrPhysicalCallLimit, BeforeSubmission: true}
		}
		return ErrPhysicalCallLimit
	}
	if err := acquirePhysicalCallBudget(ctx); err != nil {
		if ledger.before != nil {
			return &PhysicalCallAdmissionError{Err: err, BeforeSubmission: true}
		}
		return err
	}
	ledger.calls[id.ID] = &physicalEntry{fact: attemptmeta.PhysicalFact{Attempt: id, Plane: value.plane, Stage: value.stage, HeaderOutcome: "pending", BodyOutcome: "pending"}}
	ledger.order = append(ledger.order, id.ID)
	ledger.started++
	return nil
}

func recordPhysicalExchange(ctx context.Context, response *http.Response, err error) {
	value := physicalCallFromContext(ctx)
	id := attemptmeta.FromContext(ctx)
	if response != nil && attemptmeta.FromResponse(response).ID != "" {
		id = attemptmeta.FromResponse(response)
	}
	if value.trace == nil || id.ID == "" {
		return
	}
	ledger := &value.trace.ledger
	ledger.mu.Lock()
	entry := ledger.calls[id.ID]
	if entry == nil {
		ledger.mu.Unlock()
		return
	}
	entry.fact.HeaderOutcome = physicalCallOutcome(response, err)
	if response != nil {
		entry.fact.Status = response.StatusCode
	}
	if response == nil || response.Body == nil || err != nil {
		entry.fact.BodyOutcome = "unavailable"
		entry.fact.At = time.Now().UTC()
		entry.fact.DurationMS = time.Since(id.StartedAt).Milliseconds()
		entry.finalized = true
	}
	ledger.mu.Unlock()
	if response != nil && response.Body != nil {
		response.Body = &physicalBody{ReadCloser: response.Body, ledger: ledger, id: id.ID, ctx: ctx}
	}
}

type physicalBody struct {
	ctx context.Context
	io.ReadCloser
	ledger *physicalLedger
	id     string
	once   sync.Once
	err    error
	readMu sync.Mutex
}

func (b *physicalBody) Read(p []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	n, err := b.ReadCloser.Read(p)
	err = MarkPhysicalExecutionError(b.ctx, err)
	b.ledger.mu.Lock()
	defer b.ledger.mu.Unlock()
	entry := b.ledger.calls[b.id]
	if entry != nil && !entry.persisted {
		entry.fact.BodyBytes += int64(n)
		if err == io.EOF {
			entry.fact.BodyOutcome = "eof"
		} else if err != nil {
			entry.fact.BodyOutcome = "read_error"
		}
	}
	return n, err
}
func (b *physicalBody) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.readMu.Lock()
		defer b.readMu.Unlock()
		b.ledger.mu.Lock()
		defer b.ledger.mu.Unlock()
		entry := b.ledger.calls[b.id]
		if entry != nil && !entry.persisted {
			if entry.fact.BodyOutcome == "pending" {
				entry.fact.BodyOutcome = "closed_before_eof"
			}
			entry.fact.At = time.Now().UTC()
			entry.fact.DurationMS = time.Since(entry.fact.Attempt.StartedAt).Milliseconds()
			entry.finalized = true
		}
	})
	return b.err
}

// ObservePhysicalPayload receives the decompressed canonical payload. It only
// accepts root response usage, never a nested user/tool field with that name.
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
	parsed := jsonpeek.TokenUsageObject(usage)
	observePhysicalUsage(ctx, id, parsed, true)
}

// ObservePhysicalUsage is a fallback for adapters without a canonical payload
// observer. Client protocol conversion can omit counters and must not overwrite
// usage already observed on the physical response.
func ObservePhysicalUsage(ctx context.Context, id string, usage jsonpeek.TokenUsage) {
	observePhysicalUsage(ctx, id, usage, false)
}

// ObserveCanonicalPhysicalUsage accepts counters computed by the adapter from
// its native protocol. Client conversion cannot replace this observation.
func ObserveCanonicalPhysicalUsage(ctx context.Context, id string, usage jsonpeek.TokenUsage) {
	observePhysicalUsage(ctx, id, usage, true)
}

// ObservePhysicalGeneration stores the adapter's protocol observation without
// interpreting it as admission, delivery or a billing decision.
func ObservePhysicalGeneration(ctx context.Context, id, outcome string) {
	if outcome != "completed" && outcome != "failed" {
		return
	}
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return
	}
	ledger := &value.trace.ledger
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if entry := ledger.calls[id]; entry != nil && !entry.persisted {
		entry.fact.GenerationOutcome = outcome
	}
}

func observePhysicalUsage(ctx context.Context, id string, usage jsonpeek.TokenUsage, canonical bool) {
	if !usage.Found {
		return
	}
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return
	}
	ledger := &value.trace.ledger
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if entry := ledger.calls[id]; entry != nil && !entry.persisted && (canonical || !entry.canonicalUsage) {
		entry.fact.Usage = usage
		entry.canonicalUsage = canonical
	}
}

// PhysicalFacts excludes an optional response that is being handed to the
// client. Its body may already be buffered and closed, but fallback usage is
// only available when delivery finalizes.
func PhysicalFacts(ctx context.Context, pendingID ...string) []attemptmeta.PhysicalFact {
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return nil
	}
	ledger := &value.trace.ledger
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	facts := make([]attemptmeta.PhysicalFact, 0, len(ledger.order))
	for _, id := range ledger.order {
		if len(pendingID) > 0 && id == pendingID[0] {
			continue
		}
		entry := ledger.calls[id]
		if entry.finalized && !entry.persisted {
			facts = append(facts, entry.fact)
		}
	}
	return facts
}
func ConfirmPhysicalFacts(ctx context.Context, facts []attemptmeta.PhysicalFact) {
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return
	}
	ledger := &value.trace.ledger
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for _, fact := range facts {
		if entry := ledger.calls[fact.Attempt.ID]; entry != nil {
			entry.persisted = true
			if ledger.pruneConfirmed {
				delete(ledger.calls, fact.Attempt.ID)
			}
		}
	}
	if ledger.pruneConfirmed {
		remaining := ledger.order[:0]
		for _, id := range ledger.order {
			if ledger.calls[id] != nil {
				remaining = append(remaining, id)
			}
		}
		clear(ledger.order[len(remaining):])
		ledger.order = remaining
	}
}

// PhysicalObservations retains request facts independently of pending receipt
// writes. Acknowledging a receipt does not erase upstream usage. Callers that
// need a terminal snapshot must first close/join all response producers.
func PhysicalObservations(ctx context.Context) []attemptmeta.PhysicalFact {
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return nil
	}
	ledger := &value.trace.ledger
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	facts := make([]attemptmeta.PhysicalFact, 0, len(ledger.order))
	for _, id := range ledger.order {
		facts = append(facts, ledger.calls[id].fact)
	}
	return facts
}
