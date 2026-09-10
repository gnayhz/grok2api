package relational

import (
	"context"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"gorm.io/gorm"
)

const nodeFactColumns = "id, name, enabled, proxy_pool, rotation_enabled, encrypted_proxy_url, cooldown_until, exit_ip, probe_revision"

func nodeFactsFromRow(row egressNodeModel) egress.NodeFacts {
	node := toEgressDomain(row)
	return egress.NodeFacts{ID: node.ID, Name: node.Name, Enabled: node.Enabled,
		ProxyPool: node.ProxyPool, RotationEnabled: node.RotationEnabled,
		CanServeFixedTarget: egress.CanNodeServeFixedTarget(node), CooldownUntil: node.CooldownUntil,
		ExitIP: node.ExitIP, ProbeRevision: row.ProbeRevision}
}

func (r *EgressRepository) ListNodeFacts(ctx context.Context) ([]egress.NodeFacts, error) {
	var rows []egressNodeModel
	if err := r.db.db.WithContext(ctx).Select(nodeFactColumns).Order("id").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	facts := make([]egress.NodeFacts, 0, len(rows))
	for _, row := range rows {
		facts = append(facts, nodeFactsFromRow(row))
	}
	return facts, nil
}

func (r *EgressRepository) NodeFacts(ctx context.Context, id uint64) (egress.NodeFacts, bool, error) {
	var row egressNodeModel
	err := r.db.db.WithContext(ctx).Select(nodeFactColumns).Where("id = ?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return egress.NodeFacts{}, false, nil
	}
	if err != nil {
		return egress.NodeFacts{}, false, mapError(err)
	}
	return nodeFactsFromRow(row), true, nil
}
