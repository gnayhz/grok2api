package repository

import (
	"context"
	"io"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

// MediaAssetListQuery 表示管理端媒体资源列表的查询条件。
type MediaAssetListQuery struct {
	Page PageQuery
}

// MediaJobListFilter 表示视频任务列表允许使用的业务筛选条件。
type MediaJobListFilter struct {
	Status string
}

// MediaJobListQuery 表示管理端视频任务列表的查询条件。
type MediaJobListQuery struct {
	Page   PageQuery
	Filter MediaJobListFilter
}

// MediaAssetStats 表示媒体资源的聚合统计结果。
type MediaAssetStats struct {
	TotalImages int64
	TotalBytes  int64
}

// MediaUploadTicket 表示上游视频 PUT 的一次性票据元数据（不含明文 token）。
type MediaUploadTicket struct {
	TokenHash   string
	AssetID     string
	JobID       string
	MaxBytes    int64
	AllowedMIME string
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
	CreatedAt   time.Time
}

// MediaJobStats 表示各状态视频任务的聚合统计结果。
type MediaJobStats struct {
	TotalJobs  int64
	Completed  int64
	Failed     int64
	InProgress int64
	Queued     int64
}

type MediaJobRepository interface {
	StartMediaJobExecutionLimits(ctx context.Context, id, claim string, limits media.ExecutionLimits) error
	ReserveMediaJobPhysicalCall(ctx context.Context, id, claim string, now time.Time) error
	ConfirmMediaJobPhysicalCalls(ctx context.Context, id, claim string, previous, confirmed uint32) error
	CreateMediaJob(ctx context.Context, value media.Job) error
	GetMediaJob(ctx context.Context, id string, clientKeyID uint64) (media.Job, error)
	GetMediaJobsByIDs(ctx context.Context, ids []string) ([]media.Job, error)
	// UpdateMediaJob advances active jobs under claim/revision fencing. The first
	// terminal state freezes completion facts; same-terminal retries are no-ops.
	// Usage acknowledgement is only written by MarkMediaJobUsageRecorded.
	UpdateMediaJob(ctx context.Context, value media.Job) error
	// SaveMediaJobExecution advances the checkpoint under the current claim and previous revision.
	SaveMediaJobExecution(ctx context.Context, value media.Job, previous media.VideoExecution) error
	// SaveMediaJobAccessPolicy adopts an explicit grant once for a legacy job.
	// Only the current claim may replace missing policy; ordinary updates cannot.
	SaveMediaJobAccessPolicy(ctx context.Context, id, claimToken string, policy media.JobAccessPolicy) error
	// DeleteMediaJob enforces media.Job.CheckDeletion and atomically revokes
	// associated upload tickets. Active/pending jobs return ErrConflict.
	DeleteMediaJob(ctx context.Context, id string) error
	ListMediaJobs(ctx context.Context, query MediaJobListQuery) ([]media.Job, int64, error)
	SummarizeMediaJobs(ctx context.Context) (MediaJobStats, error)
	ListRecoverableMediaJobs(ctx context.Context, limit int) ([]media.Job, error)
	ListUnrecordedTerminalMediaJobs(ctx context.Context, limit int) ([]media.Job, error)
	TryClaimMediaJob(ctx context.Context, id string, now, leaseUntil time.Time, claimToken string) (media.Job, bool, error)
	MarkMediaJobUsageRecorded(ctx context.Context, id string, recordedAt time.Time) error
	ListUnrecordedMediaJobQuotas(ctx context.Context, afterID string, limit int) ([]media.Job, error)
	MarkMediaJobQuotaRecorded(ctx context.Context, value media.Job, recordedAt time.Time) error
}

// MaxMediaAssetLookupKeys bounds a single indexed metadata lookup.
const MaxMediaAssetLookupKeys = 200

// MediaAssetRepository 定义媒体资源元数据持久化能力。
type MediaAssetRepository interface {
	// CreateMediaAsset rejects private input assets; those require capacity admission.
	// Assets with SourceJobID are registered atomically against an active source
	// job. Final result selection and execution claims remain owned by the job.
	CreateMediaAsset(ctx context.Context, value media.Asset) error
	// CreateMediaInputAsset serializes input admissions and inserts only when the
	// committed total plus this asset fits the caller-owned capacity limit.
	CreateMediaInputAsset(ctx context.Context, value media.Asset, capacityLimit int64) error
	GetMediaAsset(ctx context.Context, id string) (media.Asset, error)
	// ListMediaAssetsBySourceJob returns one bounded page for terminal job
	// deletion. Delete that page before requesting the next; no offset is needed.
	ListMediaAssetsBySourceJob(ctx context.Context, jobID string, limit int) ([]media.Asset, error)
	// FindMediaAssetStorageKeys returns the currently referenced subset of exact
	// storage keys. An error never confirms absence. At most MaxMediaAssetLookupKeys
	// keys are accepted; an empty input returns an empty set without a query.
	FindMediaAssetStorageKeys(ctx context.Context, keys []string) (map[string]struct{}, error)
	ListMediaAssets(ctx context.Context, query MediaAssetListQuery) ([]media.Asset, int64, error)
	SummarizeMediaAssets(ctx context.Context) (MediaAssetStats, error)
	TotalMediaAssetBytes(ctx context.Context) (int64, error)
	// ListOldestMediaAssets 按 created_at ASC, id ASC 分页；offset 用于跳过已扫描的受保护资产。
	ListOldestMediaAssets(ctx context.Context, offset, limit int) ([]media.Asset, error)
	// ListExpiredMediaAssets 按过期时间稳定分页返回已过期临时输入，持久资产永不包含在结果中。
	ListExpiredMediaAssets(ctx context.Context, before time.Time, offset, limit int) ([]media.Asset, error)
	// ExpireMediaInputIfUnreferenced 在没有活动任务引用时原子地将临时输入标记为过期。
	ExpireMediaInputIfUnreferenced(ctx context.Context, id string, expiresAt time.Time) (bool, error)
	DeleteMediaAsset(ctx context.Context, id string) error
	// ListProtectedMediaAssetIDs 返回活动任务或未消费票据绑定的资产 ID，清理时必须跳过。
	ListProtectedMediaAssetIDs(ctx context.Context) (map[string]struct{}, error)
}

// MediaUploadTicketRepository 定义视频上传票据的持久化能力。
type MediaUploadTicketRepository interface {
	CreateUploadTicket(ctx context.Context, ticket MediaUploadTicket) error
	GetUploadTicketByHash(ctx context.Context, tokenHash string) (MediaUploadTicket, error)
	// ConsumeUploadTicket 原子消费票据；已消费或过期返回 false。
	ConsumeUploadTicket(ctx context.Context, tokenHash string, now time.Time) (MediaUploadTicket, bool, error)
	// ReleaseUploadTicket 在尚未登记资产时撤销消费，允许同票据重试。
	// 仅当票据当前为已消费状态时清除 consumed_at；返回是否成功释放。
	ReleaseUploadTicket(ctx context.Context, tokenHash string) (bool, error)
	// DeleteUploadTicketByHash 按 token_hash 精确删除票据；行不存在时幂等成功。
	// 用于签发过程中 bind 失败后的补偿回滚，不得按 job/asset 批量删除。
	DeleteUploadTicketByHash(ctx context.Context, tokenHash string) error
	// DeleteUploadTicketsByJobID 撤销指定任务尚存的上传入口；行不存在时幂等成功。
	DeleteUploadTicketsByJobID(ctx context.Context, jobID string) error
	DeleteExpiredUploadTickets(ctx context.Context, before time.Time, limit int) (int64, error)
	// BindLegacyJobResultAsset only supports active tasks without execution checkpoints.
	// New tasks bind outputs through SaveMediaJobExecution; late uploads cannot rewrite them.
	BindLegacyJobResultAsset(ctx context.Context, jobID, assetID string) error
}

// MediaObjectStorage 定义媒体二进制对象的存取边界。
type MediaObjectStorage interface {
	SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error)
	BeginVideoUpload(ctx context.Context, id, mimeType string) (MediaVideoUpload, error)
	Open(ctx context.Context, storageKey string) (io.ReadCloser, error)
	Delete(ctx context.Context, storageKey string) error
}

// MediaVideoUpload owns a single staged object's resources. The caller writes
// sequentially, validates the content, and either commits or aborts. Commit
// publishes without replacing an existing object and returns its storage key.
// Abort must always be called, including after success, to release staging
// resources; it never deletes a committed object. No method is concurrent-safe.
// The driver owns file paths/descriptors and observes Begin's context on writes
// and Commit's context on publication. Commit and Abort are idempotent.
type MediaVideoUpload interface {
	io.Writer
	Commit(ctx context.Context) (storageKey string, err error)
	Abort(ctx context.Context) error
}
