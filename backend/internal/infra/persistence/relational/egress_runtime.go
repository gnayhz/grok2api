package relational

import (
	"context"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// The request projection has one SQL owner. Adding administrator display or
// maintenance fields to Node does not silently add joins or credentials to the
// routing path. Scope and source/pool display associations are not loaded here.
var runtimeNodeColumns = []string{
	"id", "name", "enabled", "proxy_pool", "encrypted_proxy_url",
	"user_agent", "encrypted_cloudflare_cookie", "clearance_refreshed_at",
	"clearance_revision", "clearance_fingerprint", "clearance_binding_fingerprint", "binding_revision", "health_revision",
	"health", "failure_count", "cooldown_until", "last_error",
}

func (r *EgressRepository) GetRuntimeEgressNode(ctx context.Context, id uint64) (egress.Node, error) {
	var row egressNodeModel
	if err := r.db.db.WithContext(ctx).Select(runtimeNodeColumns).First(&row, id).Error; err != nil {
		return egress.Node{}, mapError(err)
	}
	return toEgressDomain(row), nil
}

func (r *EgressRepository) ListRuntimeEgressNodes(ctx context.Context) ([]egress.Node, error) {
	var rows []egressNodeModel
	if err := r.db.db.WithContext(ctx).Select(runtimeNodeColumns).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	nodes := make([]egress.Node, 0, len(rows))
	for _, row := range rows {
		nodes = append(nodes, toEgressDomain(row))
	}
	return nodes, nil
}

func (r *EgressRepository) ListRuntimeEgressPoolNodes(ctx context.Context, poolID uint64) ([]egress.Node, error) {
	// Explicit Scan gives priority its own name and avoids a second membership
	// query. The selected order is the pool's canonical scheduling order.
	type member struct {
		Node         egressNodeModel `gorm:"embedded"`
		PoolPriority int64
	}
	var rows []member
	columns := make([]string, 0, len(runtimeNodeColumns)+1)
	for _, column := range runtimeNodeColumns {
		columns = append(columns, "egress_nodes."+column)
	}
	columns = append(columns, "m.priority AS pool_priority")
	if err := r.db.db.WithContext(ctx).Table("egress_nodes").Select(strings.Join(columns, ",")).
		Joins("JOIN egress_pool_members m ON m.node_id = egress_nodes.id").Where("m.pool_id = ?", poolID).
		Order("m.priority > 0 DESC, m.priority ASC, egress_nodes.id ASC").Scan(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	nodes := make([]egress.Node, 0, len(rows))
	for _, row := range rows {
		node := toEgressDomain(row.Node)
		node.PoolPriority = row.PoolPriority
		nodes = append(nodes, node)
	}
	return nodes, nil
}
