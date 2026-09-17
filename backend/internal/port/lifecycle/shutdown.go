package lifecycle

import (
	"context"
	"errors"
)

// IsShutdownCancellation reports whether err is context.Canceled because the
// surrounding lifecycle context was torn down, not because the operation itself failed.
func IsShutdownCancellation(ctx context.Context, err error) bool {
	if err == nil || ctx == nil || ctx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled)
}
