package relational

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"math"
	"strings"
)

func (r *MediaJobRepository) SaveMediaJobExecution(ctx context.Context, value media.Job, previous media.VideoExecution) error {
	if value.ID == "" || value.ClaimToken == "" {
		return repository.ErrConflict
	}
	if err := media.ValidateVideoExecutionTransition(previous, value.Execution); err != nil {
		return err
	}
	if len(value.Quota.Mode) > 64 || strings.TrimSpace(value.Quota.Mode) != value.Quota.Mode || value.Quota.SnapshotVersion > math.MaxInt64 {
		return repository.ErrConflict
	}
	next := value.Execution
	updates := map[string]any{
		"execution_revision": next.Revision, "execution_phase": next.Phase, "native_route": next.Route, "native_endpoint": next.Endpoint, "native_job_id": next.NativeJobID, "upload_asset_id": next.UploadAssetID, "generated_at": next.GeneratedAt,
		"account_id": mediaJobAccountID(value.AccountID), "account_name": value.AccountName, "upstream_url": value.UpstreamURL, "content_type": value.ContentType, "updated_at": value.UpdatedAt.UTC(),
	}
	updates["result_asset_id"] = value.ResultAssetID
	if next.Phase == media.VideoExecutionFailed {
		updates["error_code"] = value.ErrorCode
		updates["error_message"] = value.ErrorMessage
	}
	query := r.db.db.WithContext(ctx).Model(&mediaJobModel{})
	if previous.Phase == "" || previous.Phase == media.VideoExecutionReady {
		if value.Quota.AccountID != 0 && value.Quota.AccountID != value.AccountID {
			return repository.ErrConflict
		}
		updates["quota_account_id"] = value.Quota.AccountID
		updates["quota_mode"], updates["quota_snapshot_version"] = value.Quota.Mode, value.Quota.SnapshotVersion
	} else {
		query = query.Where("quota_mode = ? AND quota_snapshot_version = ? AND quota_account_id = ?", value.Quota.Mode, value.Quota.SnapshotVersion, value.Quota.AccountID)
	}
	// A new ready attempt may select an account. Once submission is authorized,
	// every native acknowledgement and output remains bound to that same account.
	if previous.Phase != "" && previous.Phase != media.VideoExecutionReady {
		if value.AccountID == 0 {
			query = query.Where("account_id IS NULL")
		} else {
			query = query.Where("account_id = ?", value.AccountID)
		}
	}
	result := query.
		Where("id = ? AND claim_token = ? AND status = ? AND execution_revision = ? AND execution_phase = ?", value.ID, value.ClaimToken, media.StatusInProgress, previous.Revision, previous.Phase).
		Where("native_route = ? AND native_endpoint = ? AND native_job_id = ? AND upload_asset_id = ?", previous.Route, previous.Endpoint, previous.NativeJobID, previous.UploadAssetID).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return repository.ErrConflict
	}
	return nil
}
