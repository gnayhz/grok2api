package egress

import (
	"context"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

// ErrPhysicalCallLimit reports a saturated physical-call ledger.
var ErrPhysicalCallLimit = physical.ErrPhysicalCallLimit

func WithPhysicalCallPlane(ctx context.Context, plane string) context.Context {
	return physical.WithPhysicalCallPlane(ctx, plane)
}

func WithPhysicalCallStage(ctx context.Context, stage string) context.Context {
	return physical.WithPhysicalCallStage(ctx, stage)
}

// exchangeStatus derives the header status for journal recording; a missing
// response reports -1.
func exchangeStatus(response *http.Response) int {
	if response == nil {
		return -1
	}
	return response.StatusCode
}

// recordPhysicalCall records one transport exchange against the
// execution-owned journal and wraps the response body for accounting.
func recordPhysicalCall(ctx context.Context, response *http.Response, err error) {
	physical.RecordPhysicalCall(ctx, exchangeStatus(response), err)
	recordPhysicalBody(ctx, response)
}

// RecordDirectPhysicalCall records a transport call that intentionally
// bypasses the managed egress lease because no Build node is configured.
func RecordDirectPhysicalCall(ctx context.Context, response *http.Response, err error) {
	recordPhysicalCall(ctx, response, err)
}

// recordPhysicalExchange records the header outcome without touching the body
// owner (handshake paths retain their own body handling).
func recordPhysicalExchange(ctx context.Context, response *http.Response, err error) {
	if journal := physical.JournalFromContext(ctx); journal != nil {
		journal.RecordExchange(ctx, exchangeStatus(response), err)
	}
}

// recordPhysicalCallMetric emits only the per-call metric.
func recordPhysicalCallMetric(ctx context.Context, response *http.Response, err error) {
	if journal := physical.JournalFromContext(ctx); journal != nil {
		journal.RecordMetric(ctx, exchangeStatus(response), err)
	}
}

// BeginDirectPhysicalCall reserves a ledger entry for a transport call that
// bypasses the managed egress lease.
func BeginDirectPhysicalCall(ctx context.Context) error { return physical.BeginPhysicalCall(ctx) }

func beginPhysicalCall(ctx context.Context) error { return physical.BeginPhysicalCall(ctx) }
