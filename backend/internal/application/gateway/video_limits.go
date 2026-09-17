package gateway

import (
	"context"
	"log/slog"

	"github.com/chenyme/grok2api/backend/internal/application/mediajob"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func (s *Service) videoPhysicalLimits() *mediajob.Limits {
	return &mediajob.Limits{
		Journals: s.physicalJournals,
		Store:    s.mediaJobs,
		Sink: func() mediajob.PhysicalEventSink {
			if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
				if sink, ok := recorder.value.(PhysicalEventRecorder); ok {
					return sink
				}
			}
			return nil
		},
		LogError: func(jobID string, err error) {
			logger := s.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Error("video_physical_receipt_failed", "job_id", jobID, "error", err)
		},
	}
}

func (s *Service) startVideoLimits(ctx context.Context, job *media.Job) (context.Context, error) {
	return s.videoPhysicalLimits().Start(ctx, job, videoJobTimeout)
}

func (s *Service) finishVideoPhysicalReceipt(ctx context.Context, job media.Job) string {
	return s.videoPhysicalLimits().FinishReceipt(ctx, job)
}

func videoLocalExecutionFailure(ctx context.Context, err error) (string, bool) {
	return mediajob.LocalExecutionFailure(ctx, err)
}
