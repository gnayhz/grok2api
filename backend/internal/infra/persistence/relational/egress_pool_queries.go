package relational

// 代理池查询存储实现(事务聚合):池列表/详情/池成员与成员映射。
// 节点查询与生命周期写在 egress_repository.go;连通与健康在
// egress_health.go;池图在 egress_pool_graph.go。

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func (r *EgressRepository) EgressPoolMembers(ctx context.Context) (map[uint64][]uint64, error) {
	type row struct {
		PoolID uint64
		NodeID uint64
	}
	var rows []row
	if err := r.db.db.WithContext(ctx).Model(&egressPoolMemberModel{}).
		Select("pool_id, node_id").Order("pool_id, (priority > 0) DESC, priority ASC, node_id ASC").Scan(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	result := make(map[uint64][]uint64, len(rows))
	for _, item := range rows {
		result[item.PoolID] = append(result[item.PoolID], item.NodeID)
	}
	return result, nil
}

func (r *EgressRepository) ListEgressNodesByPool(ctx context.Context, poolID uint64) ([]egress.Node, error) {
	var rows []egressNodeModel
	if err := r.db.db.WithContext(ctx).
		Joins("JOIN egress_pool_members m ON m.node_id = egress_nodes.id").
		Where("m.pool_id = ?", poolID).
		Order("m.priority > 0 DESC, m.priority ASC, egress_nodes.id ASC").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	// priority 单独查一次: embedded struct + join select 在 sqlite 驱动下映射不稳。
	var memberRows []egressPoolMemberModel
	if err := r.db.db.WithContext(ctx).Where("pool_id = ?", poolID).Find(&memberRows).Error; err != nil {
		return nil, mapError(err)
	}
	priorities := make(map[uint64]int64, len(memberRows))
	for _, row := range memberRows {
		priorities[row.NodeID] = row.Priority
	}
	nodes := make([]egress.Node, 0, len(rows))
	for _, row := range rows {
		node := toEgressDomain(row)
		node.PoolPriority = priorities[row.ID]
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func (r *EgressRepository) GetEgressPool(ctx context.Context, id uint64) (egress.Pool, error) {
	var row egressPoolModel
	if err := r.db.db.WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		return egress.Pool{}, mapError(err)
	}
	return toPoolDomain(row), nil
}

func (r *EgressRepository) ListEgressPools(ctx context.Context) ([]egress.Pool, error) {
	var rows []egressPoolModel
	if err := r.db.db.WithContext(ctx).Model(&egressPoolModel{}).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	pools := make([]egress.Pool, 0, len(rows))
	for _, row := range rows {
		pools = append(pools, toPoolDomain(row))
	}
	return pools, nil
}
