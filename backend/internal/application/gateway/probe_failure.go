package gateway

import (
	"context"
	"errors"
	"io"

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

func probeAdmissionFailure(ctx context.Context, err error) model.ProbeFailure {
	if kind := probeOperationFailure(ctx, err, model.ProbeFailureAdmission); kind != model.ProbeFailureAdmission {
		return kind
	}
	switch {
	case errors.Is(err, errQualityCreatedTimeout):
		return model.ProbeFailureCreatedTimeout
	case errors.Is(err, errQualityEvidenceTimeout):
		return model.ProbeFailureEvidenceTimeout
	case errors.Is(err, errQualityEmptyStream):
		return model.ProbeFailureEmptyStream
	case errors.Is(err, io.ErrUnexpectedEOF):
		return model.ProbeFailureTruncatedStream
	case errors.Is(err, errQualityChoices), errors.Is(err, errQualityBodyShape):
		return model.ProbeFailureProtocol
	}
	return model.ProbeFailureAdmission
}
