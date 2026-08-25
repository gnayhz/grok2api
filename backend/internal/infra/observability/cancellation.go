package observability

import (
	"context"
	"errors"
)

// IsShutdownCancellation reports whether err is a context.Canceled that stems
// from the surrounding context being torn down (process shutdown or task
// cancellation) rather than from the operation itself failing.
//
// Background tasks and startup reconcilers run with a lifecycle context. When
// that context is canceled mid-operation, database and network calls surface
// context.Canceled, which must not be logged as task failure (WARN/ERROR):
// that misclassifies normal shutdown as faults and pollutes log-based
// alerting. Per-run deadline overruns (context.DeadlineExceeded) and genuine
// errors remain reportable.
func IsShutdownCancellation(ctx context.Context, err error) bool {
	if err == nil || ctx == nil || ctx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled)
}
