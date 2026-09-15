package registry

import (
	"context"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// ObserveExitIdentity consumes the version allocated before the source
// measurement. Its high-water mark and any epoch/restriction changes commit
// together. Even an unchanged identity advances the mark, fencing older
// observations after restart.
//
// 身份是双地址族的:任一非空族变化即翻 epoch(与 egress 轮换验证同一把
// 尺子)。旧档案缺失的一族首次观测到时只采纳补写基线,不翻 epoch。
func (r *Registry) ObserveExitIdentity(ctx context.Context, nodeID uint64, identity model.ExitIdentity, revision uint64) (oldEpoch, newEpoch uint64, released []model.EpochKey, err error) {
	if nodeID == 0 {
		return 0, 0, nil, ErrInvalidNode
	}
	if revision == 0 || !identity.Present() {
		return 0, 0, nil, fmt.Errorf("quality: exit observation requires an identity and revision")
	}
	if !r.inTransition {
		err = r.withTransition(ctx, func(w *Registry) error {
			oldEpoch, newEpoch, released, err = w.ObserveExitIdentity(ctx, nodeID, identity, revision)
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
		err = r.RecordExitIdentity(ctx, nodeID, identity)
	} else {
		newEpoch, released, err = r.AdvanceEpoch(ctx, nodeID, identity)
	}
	if err != nil {
		return
	}
	err = r.db.WithContext(ctx).Model(&qNodeEpochModel{}).Where("node_id = ?", nodeID).Update("observation_revision", revision).Error
	return
}
