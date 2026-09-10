package registry

import (
	"context"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// ObserveExitIP consumes the version allocated before the source measurement.
// Its high-water mark and any epoch/restriction changes commit together. Even
// an unchanged IP advances the mark, fencing older observations after restart.
func (r *Registry) ObserveExitIP(ctx context.Context, nodeID uint64, ip string, revision uint64) (oldEpoch, newEpoch uint64, released []model.EpochKey, err error) {
	if nodeID == 0 {
		return 0, 0, nil, ErrInvalidNode
	}
	if revision == 0 || ip == "" {
		return 0, 0, nil, fmt.Errorf("quality: exit observation requires an IP and revision")
	}
	if !r.inTransition {
		err = r.withTransition(ctx, func(w *Registry) error {
			oldEpoch, newEpoch, released, err = w.ObserveExitIP(ctx, nodeID, ip, revision)
			return err
		})
		return
	}
	oldEpoch = r.snapshot.load().nodeEpoch[nodeID]
	newEpoch = oldEpoch
	var current qNodeEpochModel
	if err = r.db.WithContext(ctx).Where("node_id = ?", nodeID).Find(&current).Error; err != nil {
		return
	}
	if revision <= current.ObservationRevision {
		return
	}
	if current.NodeID == 0 {
		err = r.RecordExitIP(ctx, nodeID, ip)
	} else {
		newEpoch, released, err = r.AdvanceEpoch(ctx, nodeID, ip)
	}
	if err != nil {
		return
	}
	err = r.db.WithContext(ctx).Model(&qNodeEpochModel{}).Where("node_id = ?", nodeID).Update("observation_revision", revision).Error
	return
}
