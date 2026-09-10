package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func (r *EgressRepository) ApplyEgressClearance(ctx context.Context, v egress.ClearanceUpdate) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).
		Where("id = ? AND encrypted_proxy_url = ? AND binding_revision = ? AND clearance_revision = ?", v.NodeID, v.EncryptedProxyURL, v.BindingRevision, v.ExpectedRevision).
		Updates(map[string]any{
			"clearance_revision":          gorm.Expr("clearance_revision + 1"),
			"encrypted_cloudflare_cookie": v.EncryptedCookie, "user_agent": v.UserAgent,
			"clearance_fingerprint": v.Fingerprint, "clearance_binding_fingerprint": v.BindingFingerprint,
			"clearance_refreshed_at": v.RefreshedAt, "updated_at": time.Now().UTC(),
			"last_error": gorm.Expr("CASE WHEN last_error = ? THEN '' ELSE last_error END", "clearance refresh failed"),
		})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (r *EgressRepository) RecordEgressClearanceError(ctx context.Context, node egress.Node) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).
		Where("id = ? AND encrypted_proxy_url = ? AND binding_revision = ? AND clearance_revision = ?", node.ID, node.EncryptedProxyURL, node.BindingRevision, node.ClearanceRevision).
		Where("last_error = '' OR last_error = ?", "clearance refresh failed").
		Updates(map[string]any{"last_error": "clearance refresh failed", "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrConflict
	}
	return nil
}
