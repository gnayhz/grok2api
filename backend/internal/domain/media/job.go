package media

import (
	"fmt"
	"strings"
	"time"
)

type Status string

const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// MaxInputJSONBytes is the persisted media_jobs.input_json ceiling (32 MiB).
// Keep the relational CHECK constraint and gateway encode guard aligned with this value.
const MaxInputJSONBytes = 32 << 20

// MaxInputImages is the maximum number of reference images accepted for a video job.
const MaxInputImages = 8

// MaxReferenceAudios is the maximum number of reference audio tracks accepted
// for a video job.
const MaxReferenceAudios = 3

// MaxInputAssetBytes limits each temporary image or video input to 20 MiB.
const MaxInputAssetBytes = 20 << 20

// ValidateVideoGenerationInput 是视频生成输入组合约束的唯一规则
// (transport 的协议级 400 快速拒绝与 gateway 的用例校验共用;错误文本
// 即对外消息)。resolution 为空或任意大小写形式。
func ValidateVideoGenerationInput(imagePresent bool, referenceImages, referenceAudios int, promptPresent bool, resolution string) error {
	hasReferenceMode := referenceImages > 0 || referenceAudios > 0
	if imagePresent && hasReferenceMode {
		return fmt.Errorf("image 不能与 reference_images/reference_audios 同时使用")
	}
	if referenceAudios > MaxReferenceAudios {
		return fmt.Errorf("reference_audios 最多 %d 个", MaxReferenceAudios)
	}
	if referenceImages > MaxInputImages {
		return fmt.Errorf("reference_images 不能超过 %d 张", MaxInputImages)
	}
	if hasReferenceMode {
		if !promptPresent {
			return fmt.Errorf("参考图/参考音频视频必须提供 prompt")
		}
		if strings.EqualFold(strings.TrimSpace(resolution), "1080p") {
			return fmt.Errorf("参考图视频 resolution 最高 720p")
		}
	}
	if !promptPresent && !imagePresent && !hasReferenceMode {
		return fmt.Errorf("文本生视频必须提供 prompt；图片生视频可以省略 prompt")
	}
	return nil
}

// ValidateVideoReferenceAudios 校验参考音频声轨:数量上限与逐条非空。
func ValidateVideoReferenceAudios(values []string) error {
	if len(values) > MaxReferenceAudios {
		return fmt.Errorf("reference_audios 最多 %d 个", MaxReferenceAudios)
	}
	for _, raw := range values {
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("reference_audios.voice_id 不能为空")
		}
	}
	return nil
}

type VideoOperation string

const (
	VideoOperationGenerate VideoOperation = "generate"
	VideoOperationEdit     VideoOperation = "edit"
	VideoOperationExtend   VideoOperation = "extend"
)

// Job 表示可跨进程重启恢复的异步视频任务。
type Job struct {
	ID            string
	RequestID     string
	ClientKeyID   uint64
	ClientKeyName string
	ClientIP      string
	AccessPolicy  JobAccessPolicy
	Execution     VideoExecution
	Limits        ExecutionLimits
	Quota         JobQuota
	// ClaimedFromStatus is ephemeral claim metadata, never persisted as a second status.
	ClaimedFromStatus Status
	AccountID         uint64
	AccountName       string
	EgressNodeID      *uint64
	EgressNodeName    string
	EgressScope       string
	EgressMode        string
	Provider          string
	Model             string
	ModelRouteID      uint64
	UpstreamModel     string
	Operation         VideoOperation
	Prompt            string
	Seconds           int
	Size              string
	Quality           string
	Status            Status
	Progress          int
	InputJSON         string
	InputImageCount   int
	UpstreamURL       string
	// ResultAssetID 指向本地媒体资产；XAI ZDR 上传完成后优先从此读取。
	ResultAssetID   string
	ContentType     string
	ErrorCode       string
	ErrorMessage    string
	LeaseUntil      *time.Time
	ClaimToken      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CompletedAt     *time.Time
	UsageRecordedAt *time.Time
}

// JobQuota is the selected quota snapshot and its durable handoff to M07.
// It does not own quota arithmetic, upstream windows, or refresh policy.
type JobQuota struct {
	AccountID       uint64
	Mode            string
	SnapshotVersion uint64
	RecordedAt      *time.Time
}

// PendingQuotaHandoff protects the durable generation source until M07 has
// accepted its consumption, including while a failed archive is terminal.
func (j Job) PendingQuotaHandoff() bool {
	return j.Quota.RecordedAt == nil && (j.Execution.Phase == VideoExecutionGenerated || j.Execution.Phase == "" && j.Status == StatusCompleted)
}
