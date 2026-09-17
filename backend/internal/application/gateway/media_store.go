package gateway

import (
	"context"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

// JobExecutionStore 是 gateway 媒体执行所需的作业仓储能力：创建、认领、
// 状态推进、配额/用量记账标记、访问政策与物理预算预留/确认。作业删除、
// 归档、列表查询与清理不在执行合同内。
type JobExecutionStore interface {
	CreateMediaJob(ctx context.Context, value mediadomain.Job) error
	ListRecoverableMediaJobs(ctx context.Context, limit int) ([]mediadomain.Job, error)
	ListUnrecordedTerminalMediaJobs(ctx context.Context, limit int) ([]mediadomain.Job, error)
	TryClaimMediaJob(ctx context.Context, id string, now, leaseUntil time.Time, claimToken string) (mediadomain.Job, bool, error)
	UpdateMediaJob(ctx context.Context, value mediadomain.Job) error
	SaveMediaJobExecution(ctx context.Context, value mediadomain.Job, previous mediadomain.VideoExecution) error
	SaveMediaJobAccessPolicy(ctx context.Context, id, claimToken string, policy mediadomain.JobAccessPolicy) error
	MarkMediaJobUsageRecorded(ctx context.Context, id string, recordedAt time.Time) error
	ListUnrecordedMediaJobQuotas(ctx context.Context, afterID string, limit int) ([]mediadomain.Job, error)
	MarkMediaJobQuotaRecorded(ctx context.Context, value mediadomain.Job, recordedAt time.Time) error
	StartMediaJobExecutionLimits(ctx context.Context, id, claim string, limits mediadomain.ExecutionLimits) error
	ReserveMediaJobPhysicalCall(ctx context.Context, id, claim string, now time.Time) error
	ConfirmMediaJobPhysicalCalls(ctx context.Context, id, claim string, previous, confirmed uint32) error
}
