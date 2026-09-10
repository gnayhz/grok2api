package registry

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// ReleaseCurrentExitAfterReview revokes all current-epoch exit restrictions,
// preserving account restrictions and the original case verdict/evidence. The
// party flag prevents later automatic settlement from reinstating this hold.
func (r *Registry) ReleaseCurrentExitAfterReview(ctx context.Context, nodeID uint64, reason string) error {
	if nodeID == 0 {
		return ErrInvalidNode
	}
	ctx, release, err := r.Coordinate(ctx, "court")
	if err != nil {
		return err
	}
	defer release()
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "operator_exit_release"
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return r.withTransition(ctx, func(w *Registry) error {
		epoch, now := w.CurrentEpoch(nodeID), time.Now().UTC()
		var parties []qCasePartyModel
		if err := w.db.WithContext(ctx).Where("kind = ? AND node_id = ? AND epoch = ? AND disposition IN ?", "exit", nodeID, epoch, []string{"remanded", "sentenced"}).Find(&parties).Error; err != nil {
			return err
		}
		for _, party := range parties {
			var record qCaseModel
			if err := w.db.WithContext(ctx).Where("id = ?", party.CaseID).Take(&record).Error; err != nil {
				return err
			}
			var envelope map[string]any
			if record.EvidenceJSON != "" {
				if err := json.Unmarshal([]byte(record.EvidenceJSON), &envelope); err != nil {
					return err
				}
			}
			if envelope == nil {
				envelope = map[string]any{}
			}
			envelope["manual_exit_release"] = map[string]any{"at": now, "node_id": nodeID, "epoch": epoch, "reason": reason, "previous_disposition": party.Disposition}
			raw, err := json.Marshal(envelope)
			if err != nil {
				return err
			}
			if err := w.db.WithContext(ctx).Model(&qCaseModel{}).Where("id = ?", party.CaseID).Updates(map[string]any{"evidence_json": string(raw), "updated_at": now}).Error; err != nil {
				return err
			}
			if err := w.db.WithContext(ctx).Model(&qCasePartyModel{}).Where("id = ?", party.ID).Updates(map[string]any{"disposition": "released", "review_released": true, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if err := w.db.WithContext(ctx).Where("node_id = ? AND epoch = ?", nodeID, epoch).Delete(&qExitStateModel{}).Error; err != nil {
			return err
		}
		next := w.snapshot.load().clone()
		delete(next.exitStates, model.EpochKey{NodeID: nodeID, Epoch: epoch})
		w.snapshot.store(next)
		return nil
	})
}
