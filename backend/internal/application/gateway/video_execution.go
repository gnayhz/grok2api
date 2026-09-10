package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func (s *Service) initializeVideoExecution(ctx context.Context, job *media.Job) error {
	if job.Execution.Phase != "" {
		return job.Execution.Validate()
	}
	phase := media.VideoExecutionUnconfirmed
	if job.ClaimedFromStatus == media.StatusQueued {
		phase = media.VideoExecutionReady
	}
	next := *job
	next.Execution = media.VideoExecution{Revision: 1, Phase: phase}
	return s.saveVideoExecution(ctx, job, next)
}

func (s *Service) checkpointVideo(ctx context.Context, job *media.Job, point provider.VideoCheckpoint) error {
	previous := job.Execution
	next := *job
	switch point.Phase {
	case media.VideoExecutionReady:
		next.Execution = media.VideoExecution{Phase: point.Phase}
		next.UpstreamURL, next.ContentType, next.ResultAssetID = "", "", ""
	case media.VideoExecutionSubmitting:
		next.Execution = media.VideoExecution{Phase: point.Phase, Route: point.Route, Endpoint: point.Endpoint, UploadAssetID: point.UploadAssetID}
	case media.VideoExecutionSubmitted:
		next.Execution.Phase, next.Execution.NativeJobID = point.Phase, point.NativeJobID
	case media.VideoExecutionFailed:
		next.Execution.Phase = point.Phase
		next.ErrorCode = "generation_failed"
		next.ErrorMessage = sanitizeDiagnosticText(point.Failure, 512)
	case media.VideoExecutionGenerated:
		next.Execution.Phase = point.Phase
		if next.Execution.GeneratedAt == nil {
			now := time.Now().UTC()
			next.Execution.GeneratedAt = &now
		}
		if point.Result.URL != "" {
			next.UpstreamURL = point.Result.URL
		}
		if point.Result.ContentType != "" {
			next.ContentType = point.Result.ContentType
		}
		if point.Result.AssetID != "" {
			next.ResultAssetID = point.Result.AssetID
		}
	default:
		return media.ErrInvalidVideoExecution
	}
	next.Execution.Revision = previous.Revision + 1
	if err := s.saveVideoExecution(ctx, job, next); err != nil {
		return err
	}
	if next.Execution.Phase == media.VideoExecutionGenerated {
		// The checkpoint remains the recovery source even if this independent
		// handoff fails. Archive/delivery cannot erase confirmed consumption.
		_ = s.finishVideoQuota(ctx, job)
	}
	return nil
}

func (s *Service) saveVideoExecution(ctx context.Context, job *media.Job, next media.Job) error {
	previous := job.Execution
	if err := media.ValidateVideoExecutionTransition(previous, next.Execution); err != nil {
		return err
	}
	next.UpdatedAt = time.Now().UTC()
	var err error
	for range 3 {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		err = s.mediaJobs.SaveMediaJobExecution(writeCtx, next, previous)
		cancel()
		if err == nil {
			*job = next
			return nil
		}
		if errors.Is(err, repository.ErrConflict) {
			return err
		}
	}
	return err
}

func videoResumeCheckpoint(job media.Job) *provider.VideoCheckpoint {
	if job.Execution.Phase != media.VideoExecutionSubmitted && job.Execution.Phase != media.VideoExecutionGenerated {
		return nil
	}
	return &provider.VideoCheckpoint{Phase: job.Execution.Phase, Route: job.Execution.Route, Endpoint: job.Execution.Endpoint, NativeJobID: job.Execution.NativeJobID, UploadAssetID: job.Execution.UploadAssetID, Result: provider.VideoResult{URL: job.UpstreamURL, ContentType: job.ContentType, AssetID: job.ResultAssetID}}
}
