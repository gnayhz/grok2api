package egress

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

func IsPhysicalCallAdmissionError(err error) bool { return physical.IsPhysicalCallAdmissionError(err) }
func MarkPhysicalExecutionError(ctx context.Context, err error) error {
	return physical.MarkPhysicalExecutionError(ctx, err)
}
