package observability

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/port/lifecycle"
)

func IsShutdownCancellation(ctx context.Context, err error) bool {
	return lifecycle.IsShutdownCancellation(ctx, err)
}
