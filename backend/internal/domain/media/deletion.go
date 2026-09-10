package media

import "errors"

var (
	ErrJobActive            = errors.New("排队中或生成中的视频任务不能删除")
	ErrJobCompletionPending = errors.New("视频任务的额度或用量记录尚未完成，请稍后再删除")
)

// CheckDeletion protects the durable source until all required completion
// handoffs have been accepted. Failed jobs also retain their usage/diagnostic
// fact, even when no generation or charge has been confirmed.
func (j Job) CheckDeletion() error {
	if j.Status != StatusCompleted && j.Status != StatusFailed {
		return ErrJobActive
	}
	if j.PendingQuotaHandoff() || j.UsageRecordedAt == nil {
		return ErrJobCompletionPending
	}
	return nil
}
