package registry

import "context"

// ExitAllowed checks the latest persisted epoch and all active owners in one
// database snapshot. It is the authoritative check at the lease boundary.
func (r *Registry) ExitAllowed(ctx context.Context, nodeID uint64) (bool, error) {
	if nodeID == 0 {
		return true, nil
	}
	var blocked bool
	err := r.db.WithContext(ctx).Raw(`SELECT EXISTS(
		SELECT 1 FROM q_exit_state s WHERE s.node_id = ? AND s.state <> 'available'
		AND s.epoch = COALESCE((SELECT epoch FROM q_node_epoch WHERE node_id = ?), 0)
		UNION ALL SELECT 1 FROM q_case_party p JOIN q_case c ON c.id = p.case_id
		WHERE p.kind = 'exit' AND p.node_id = ?
		AND p.epoch = COALESCE((SELECT epoch FROM q_node_epoch WHERE node_id = ?), 0)
		AND (p.disposition = 'sentenced' OR (p.disposition = 'remanded' AND c.status IN ('investigating', 'exit_guilty')))
	)`, nodeID, nodeID, nodeID, nodeID).Scan(&blocked).Error
	return !blocked, err
}

// CurrentEpochAt is used when labeling physical submissions and experiments;
// stale candidate caches are never accepted as authoritative path versions.
func (r *Registry) CurrentEpochAt(ctx context.Context, nodeID uint64) (uint64, bool, error) {
	var rows []qNodeEpochModel
	if err := r.db.WithContext(ctx).Where("node_id = ?", nodeID).Find(&rows).Error; err != nil {
		return 0, false, err
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	return rows[0].Epoch, true, nil
}
