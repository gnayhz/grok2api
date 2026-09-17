package egress

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

// ObservePhysicalPayload receives the decompressed canonical payload. It only
// accepts root response usage, never a nested user/tool field with that name.
func ObservePhysicalPayload(ctx context.Context, id string, data []byte) {
	physical.ObservePhysicalPayload(ctx, id, data)
}

// ObserveCanonicalPhysicalUsage accepts adapter-computed counters.
func ObserveCanonicalPhysicalUsage(ctx context.Context, id string, usage jsonpeek.TokenUsage) {
	physical.ObserveCanonicalPhysicalUsage(ctx, id, usage)
}

// ObservePhysicalGeneration stores the adapter's protocol outcome.
func ObservePhysicalGeneration(ctx context.Context, id, outcome string) {
	physical.ObservePhysicalGeneration(ctx, id, outcome)
}

// recordPhysicalBody wraps a response body so byte counts, terminal outcomes
// and close semantics feed the execution-owned journal.
func recordPhysicalBody(ctx context.Context, response *http.Response) {
	journal := physical.JournalFromContext(ctx)
	if journal == nil || response == nil || response.Body == nil {
		return
	}
	id := attemptmeta.FromResponse(response)
	if id.ID == "" {
		id = attemptmeta.FromContext(ctx)
	}
	if id.ID == "" {
		return
	}
	response.Body = &physicalBody{ReadCloser: response.Body, journal: journal, id: id.ID, ctx: ctx}
}

type physicalBody struct {
	ctx context.Context
	io.ReadCloser
	journal physical.Journal
	id      string
	once    sync.Once
	err     error
	readMu  sync.Mutex
}

func (b *physicalBody) Read(p []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	n, err := b.ReadCloser.Read(p)
	err = physical.MarkPhysicalExecutionError(b.ctx, err)
	b.journal.ObserveBody(b.id, n, bodyReadOutcome(err))
	return n, err
}

func (b *physicalBody) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.readMu.Lock()
		defer b.readMu.Unlock()
		b.journal.FinalizeBody(b.id, "closed_before_eof", startedAtFor(b))
	})
	return b.err
}

func startedAtFor(b *physicalBody) time.Time {
	return attemptmeta.FromContext(b.ctx).StartedAt
}

func bodyReadOutcome(err error) string {
	switch {
	case err == nil:
		return ""
	case err == io.EOF:
		return "eof"
	default:
		return "read_error"
	}
}
