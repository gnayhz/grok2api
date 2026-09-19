package gateway

import (
	"context"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func probeOperationFailure(ctx context.Context, err error, fallback model.ProbeFailure) model.ProbeFailure {
	switch {
	case ctx.Err() != nil, errors.Is(err, context.Canceled):
		return model.ProbeFailureInterrupted
	case errors.Is(err, responsebuffer.ErrExhausted), errors.Is(err, responsebuffer.ErrLimit), errors.Is(err, errQualityHoldLimit):
		return model.ProbeFailureResource
	case errors.Is(err, context.DeadlineExceeded):
		if fallback == model.ProbeFailureCompletion {
			return model.ProbeFailureCompletionBudget
		}
		return model.ProbeFailureInterrupted
	}
	return fallback
}
