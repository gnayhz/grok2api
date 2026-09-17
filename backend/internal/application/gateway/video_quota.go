package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func (s *Service) finishVideoQuota(ctx context.Context, job *media.Job) error {
	if !videoGenerated(*job) || job.Quota.RecordedAt != nil {
		return nil
	}
	kind := account.Provider(job.Provider)
	quotaKind, _ := s.providers.QuotaKind(kind)
	accountID := job.Quota.AccountID
	if accountID == 0 {
		accountID = job.AccountID
	}
	if quotaKind == provider.QuotaRemoteWindow && accountID != 0 {
		mode := job.Quota.Mode
		if mode == "" {
			// Legacy generated jobs did not retain a selected snapshot. The zero
			// version always requests an authoritative refresh, never subtraction.
			mode = videoQuotaMode(kind, s.providers.QuotaMode(kind, job.UpstreamModel), job.Quality)
		}
		if mode == "" {
			return fmt.Errorf("video %s quota mode unavailable", job.ID)
		}
		fact := account.QuotaConsumption{EventID: "video_quota_" + job.ID, AccountID: accountID, Mode: mode, SnapshotVersion: job.Quota.SnapshotVersion, Units: 1}
		if err := s.finishQuotaConsumption(newFinalizationBudget("video", job.Provider), fact); err != nil {
			return err
		}
		refresh, _, availability := quotaFinalizationModes(mode, s.providers.QuotaRefreshGroup(kind, job.UpstreamModel))
		s.accounts.QueueQuotaRefresh(accountID, refresh)
		if availability != "" && availability != refresh {
			s.accounts.QueueQuotaRefresh(accountID, availability)
		}
	}
	now := time.Now().UTC()
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accountStateWriteTimeout)
	defer cancel()
	if err := s.mediaJobs.MarkMediaJobQuotaRecorded(writeCtx, *job, now); err != nil {
		s.logger.Warn("video_quota_receipt_write_failed", "job_id", job.ID, "error", err)
		return err
	}
	job.Quota.RecordedAt = &now
	return nil
}

func (s *Service) reconcileVideoQuotas(ctx context.Context) error {
	if s.background == nil {
		return nil
	}
	cursor, advance, reset, finish := s.background.beginQuotaReconcile()
	if finish == nil {
		return nil
	}
	defer finish()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var result error
	jobs, err := s.mediaJobs.ListUnrecordedMediaJobQuotas(ctx, cursor, 200)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		reset()
		return nil
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		advance(job.ID)
		if err := s.finishVideoQuota(ctx, &job); err != nil {
			result = firstError(result, err)
		}
	}
	return result
}
