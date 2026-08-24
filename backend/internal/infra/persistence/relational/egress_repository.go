package relational

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type EgressRepository struct{ db *Database }

func NewEgressRepository(db *Database) *EgressRepository { return &EgressRepository{db: db} }

func (r *EgressRepository) ListEgressNodes(ctx context.Context, sort repository.SortQuery) ([]egress.Node, error) {
	var rows []egressNodeModel
	query := r.db.db.WithContext(ctx).Model(&egressNodeModel{})
	query = applyStableSort(query, sort, map[string]sortSpec{
		"name":   {expression: "LOWER(egress_nodes.name)"},
		"proxy":  {expression: "CASE WHEN egress_nodes.encrypted_proxy_url <> '' THEN 0 ELSE 1 END"},
		"health": {expression: "egress_nodes.health", defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "egress_nodes.id"}, "egress_nodes.id")
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]egress.Node, 0, len(rows))
	// SourceName/PoolIDs 是管理端列表投影;运行时调度快照(manager 每秒经此
	// 方法装快照)不需要,也无从受益。零来源/零成员是常见形态(纯手工节点
	// 库),此时两次富集查询是纯浪费——先扫行内信息,仅在有来源或成员引用
	// 时才发起对应查询。
	needsSources := false
	for _, row := range rows {
		if row.SourceID != nil {
			needsSources = true
			break
		}
	}
	var sourceNames map[uint64]string
	var nodePools map[uint64][]uint64
	var err error
	if needsSources {
		if sourceNames, err = r.egressSourceNames(ctx); err != nil {
			return nil, err
		}
	}
	if len(rows) > 0 {
		// 池成员引用无法从节点行判断,由成员表查询决定;空节点库必然无成员,
		// 直接跳过。成员查询本身有界(池成员表远小于节点表)。
		if nodePools, err = r.egressNodePoolIDs(ctx); err != nil {
			return nil, err
		}
	}
	for _, row := range rows {
		value := toEgressDomain(row)
		if sourceNames != nil {
			value.SourceName = sourceNames[value.SourceID]
		}
		if nodePools != nil {
			value.PoolIDs = nodePools[value.ID]
		}
		values = append(values, value)
	}
	return values, nil
}

func (r *EgressRepository) ListEgressNodePage(ctx context.Context, input repository.EgressNodeListQuery) ([]egress.Node, int64, error) {
	query := r.db.db.WithContext(ctx).Model(&egressNodeModel{})
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		query = query.Where("LOWER(egress_nodes.name) LIKE ?", "%"+strings.ToLower(search)+"%")
	}
	if input.Filter.Enabled != nil {
		query = query.Where("egress_nodes.enabled = ?", *input.Filter.Enabled)
	}
	if input.Filter.ProbeStatus != "" {
		query = query.Where("egress_nodes.probe_status = ?", input.Filter.ProbeStatus)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"name":   {expression: "LOWER(egress_nodes.name)"},
		"proxy":  {expression: "CASE WHEN egress_nodes.encrypted_proxy_url <> '' THEN 0 ELSE 1 END"},
		"health": {expression: "egress_nodes.health", defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "egress_nodes.id"}, "egress_nodes.id")
	var rows []egressNodeModel
	if err := query.Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 {
		return []egress.Node{}, total, nil
	}

	sourceNames, err := r.egressSourceNames(ctx)
	if err != nil {
		return nil, 0, err
	}
	nodePools, err := r.egressNodePoolIDs(ctx)
	if err != nil {
		return nil, 0, err
	}
	values := make([]egress.Node, 0, len(rows))
	for _, row := range rows {
		value := toEgressDomain(row)
		value.SourceName = sourceNames[value.SourceID]
		value.PoolIDs = nodePools[value.ID]
		values = append(values, value)
	}
	return values, total, nil
}

func (r *EgressRepository) GetEgressNode(ctx context.Context, id uint64) (egress.Node, error) {
	var row egressNodeModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return egress.Node{}, mapError(err)
	}
	value := toEgressDomain(row)
	// 单节点读取只查该节点的成员引用(索引点查),不再全表扫描池成员表——
	// 此方法在请求路径(固定目标解析)与反馈路径每次调用都会触发,此前
	// egressNodePoolIDs 的整表扫描随成员数线性放大。排序与列表路径
	// (egressNodePoolIDs←EgressPoolMembers)保持同一元组语义,两条读取
	// 路径对同一节点的 PoolIDs 投影必须一致。
	var poolIDs []uint64
	if err := r.db.db.WithContext(ctx).Model(&egressPoolMemberModel{}).
		Where("node_id = ?", id).
		Order("pool_id ASC, (priority > 0) DESC, priority ASC, node_id ASC").
		Pluck("pool_id", &poolIDs).Error; err != nil {
		return egress.Node{}, mapError(err)
	}
	value.PoolIDs = poolIDs
	return value, nil
}

func (r *EgressRepository) CreateEgressNode(ctx context.Context, value egress.Node) (egress.Node, error) {
	row := fromEgressDomain(value)
	if err := r.db.db.WithContext(ctx).Create(&row).Error; err != nil {
		return egress.Node{}, mapError(err)
	}
	return toEgressDomain(row), nil
}

func (r *EgressRepository) CreateEgressNodes(ctx context.Context, values []egress.Node) (int, error) {
	if len(values) == 0 {
		return 0, nil
	}
	rows := make([]egressNodeModel, 0, len(values))
	for _, value := range values {
		rows = append(rows, fromEgressDomain(value))
	}
	if err := r.db.db.WithContext(ctx).CreateInBatches(&rows, 100).Error; err != nil {
		return 0, mapError(err)
	}
	return len(rows), nil
}

func (r *EgressRepository) UpdateEgressNode(ctx context.Context, value egress.Node) (egress.Node, error) {
	row := fromEgressDomain(value)
	// 管理端编辑只写配置面列。此前 Select("*").Updates 全行覆盖:读-改-写窗口
	// 内后台 rotation worker / 探测 / 质量隔离的窄列写(last_rotated_at/
	// rotation_attempts/cooldown_until/last_error 等)会被陈旧快照整体回滚——
	// 典型后果是已耗尽的节点重新获得换 IP 预算、已隔离节点提前回池。运行态
	// 列由各自的窄方法(UpdateEgressNodeRotationState/Probe/QualityState)独占
	// 写入; 配置变化时的健康重置(applyInput configurationChanged)在下方以
	// 运行态全零的特征整组写入, 与旧行为一致。
	updates := map[string]any{
		"name": row.Name, "enabled": row.Enabled, "proxy_pool": row.ProxyPool,
		"encrypted_proxy_url":    row.EncryptedProxyURL,
		"encrypted_rotation_url": row.EncryptedRotationURL, "rotation_enabled": row.RotationEnabled,
		"updated_at": time.Now().UTC(),
		"source_id":  row.SourceID, "source_key": row.SourceKey,
	}
	if row.Health == 1 && row.FailureCount == 0 && row.CooldownUntil == nil && row.LastError == "" && row.ProbeStatus == string(egress.ProbeStatusUnknown) {
		// 质量隔离是守卫独占状态:读-改-写窗口内落库的隔离(cooldown +
		// exit_ip_quality)不得被配置重置覆盖,否则降智出口立即回池承流。
		// 传输层健康/探测按文档意图重置(配置变更使观测失效)。与
		// UpdateEgressNodeProbe 对传输错误的 CASE 守护同一模式。
		quarantined := "last_error = ?"
		updates["health"] = gorm.Expr("CASE WHEN "+quarantined+" THEN health ELSE 1 END", egress.LastErrorExitIPQuality)
		updates["failure_count"] = gorm.Expr("CASE WHEN "+quarantined+" THEN failure_count ELSE 0 END", egress.LastErrorExitIPQuality)
		updates["cooldown_until"] = gorm.Expr("CASE WHEN "+quarantined+" THEN cooldown_until ELSE NULL END", egress.LastErrorExitIPQuality)
		updates["last_error"] = gorm.Expr("CASE WHEN "+quarantined+" THEN last_error ELSE '' END", egress.LastErrorExitIPQuality)
		updates["probe_status"], updates["last_probed_at"] = egress.ProbeStatusUnknown, nil
		updates["probe_latency_ms"], updates["exit_ip"], updates["probe_error"], updates["probe_provider"] = 0, "", "", ""
		updates["ipv4_probe_status"], updates["ipv4_last_probed_at"], updates["ipv4_probe_latency_ms"], updates["ipv4_exit_ip"], updates["ipv4_probe_error"] = egress.ProbeStatusUnknown, nil, 0, "", ""
		updates["ipv6_probe_status"], updates["ipv6_last_probed_at"], updates["ipv6_probe_latency_ms"], updates["ipv6_exit_ip"], updates["ipv6_probe_error"] = egress.ProbeStatusUnknown, nil, 0, "", ""
		updates["degrade_count"] = gorm.Expr("CASE WHEN "+quarantined+" THEN degrade_count ELSE 0 END", egress.LastErrorExitIPQuality)
		updates["last_degraded_at"] = gorm.Expr("CASE WHEN "+quarantined+" THEN last_degraded_at ELSE NULL END", egress.LastErrorExitIPQuality)
	}
	updates["clearance_refreshed_at"], updates["clearance_fingerprint"] = nil, ""
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", row.ID).Updates(updates)
	if result.Error != nil {
		return egress.Node{}, mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return egress.Node{}, repository.ErrNotFound
	}
	return r.GetEgressNode(ctx, row.ID)
}

func (r *EgressRepository) UpdateEgressNodesEnabled(ctx context.Context, ids []uint64, enabled bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if enabled {
		result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).
			Where("id IN ? AND enabled <> ?", ids, true).
			Updates(map[string]any{"enabled": true, "updated_at": time.Now().UTC()})
		return int(result.RowsAffected), mapError(result.Error)
	}
	var updated int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		config, err := lockEgressOperationsConfig(tx)
		if err != nil {
			return err
		}
		var lockedIDs []uint64
		if err := tx.Model(&egressNodeModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ?", ids).Order("id ASC").Pluck("id", &lockedIDs).Error; err != nil {
			return err
		}
		if configReferencesAnyRoutingNode(config.Routing, lockedIDs) {
			return repository.ErrEgressRoutingNodeInUse
		}
		result := tx.Model(&egressNodeModel{}).
			Where("id IN ? AND enabled <> ?", lockedIDs, false).
			Updates(map[string]any{"enabled": false, "updated_at": time.Now().UTC()})
		updated = result.RowsAffected
		return mapError(result.Error)
	})
	return int(updated), mapError(err)
}

func (r *EgressRepository) UpdateEgressNodeClearance(ctx context.Context, id uint64, encryptedCookie, userAgent, fingerprint, bindingFingerprint string, refreshedAt time.Time) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"encrypted_cloudflare_cookie": encryptedCookie, "user_agent": userAgent,
		"clearance_fingerprint": fingerprint, "clearance_refreshed_at": refreshedAt,
		"clearance_binding_fingerprint": bindingFingerprint,
		"last_error":                    "", "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *EgressRepository) UpdateEgressNodeHealth(ctx context.Context, id uint64, health float64, failureCount int, cooldownUntil *time.Time, lastError string) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"health": health, "failure_count": failureCount, "cooldown_until": cooldownUntil, "last_error": lastError, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *EgressRepository) UpdateEgressNodeLastError(ctx context.Context, id uint64, lastError string) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"last_error": lastError, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// UpdateEgressNodeRotationURL persists the encrypted rotation webhook together
// with its enabled flag, so batch template updates cannot clobber health or
// degrade state — and a non-empty webhook always leaves rotation armed.
func (r *EgressRepository) UpdateEgressNodeRotationURL(ctx context.Context, id uint64, encryptedRotationURL string, enabled bool) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"encrypted_rotation_url": encryptedRotationURL,
		"rotation_enabled":       enabled,
		"updated_at":             time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func toPoolDomain(row egressPoolModel) egress.Pool {
	return egress.Pool{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled,
		Strategy:     egress.PoolStrategy(row.Strategy).Normalized(),
		FallbackMode: egress.PoolFallbackMode(row.FallbackMode).Normalized(), FallbackPoolID: row.FallbackPoolID,
		RotationCursorNodeID: row.RotationCursorNodeID,
		CreatedAt:            row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func fromPoolDomain(value egress.Pool) egressPoolModel {
	return egressPoolModel{
		ID: value.ID, Name: value.Name, Enabled: value.Enabled,
		Strategy:       string(value.Strategy.Normalized()),
		FallbackMode:   string(value.FallbackMode.Normalized()),
		FallbackPoolID: value.FallbackPoolID,
		CreatedAt:      value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

// ListEgressPools lists pools ordered by id.
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

func (r *EgressRepository) GetEgressPool(ctx context.Context, id uint64) (egress.Pool, error) {
	var row egressPoolModel
	if err := r.db.db.WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		return egress.Pool{}, mapError(err)
	}
	return toPoolDomain(row), nil
}

func (r *EgressRepository) CreateEgressPool(ctx context.Context, value egress.Pool) (egress.Pool, error) {
	row := fromPoolDomain(value)
	if err := r.db.db.WithContext(ctx).Create(&row).Error; err != nil {
		return egress.Pool{}, mapError(err)
	}
	return toPoolDomain(row), nil
}

func (r *EgressRepository) UpdateEgressPool(ctx context.Context, value egress.Pool) (egress.Pool, error) {
	row := fromPoolDomain(value)
	result := r.db.db.WithContext(ctx).Model(&egressPoolModel{}).Where("id = ?", value.ID).Updates(map[string]any{
		"name": row.Name, "enabled": row.Enabled, "strategy": row.Strategy,
		"fallback_mode": row.FallbackMode, "fallback_pool_id": row.FallbackPoolID,
		"updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return egress.Pool{}, mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return egress.Pool{}, repository.ErrNotFound
	}
	return r.GetEgressPool(ctx, value.ID)
}

func (r *EgressRepository) DeleteEgressPool(ctx context.Context, id uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("pool_id = ?", id).Delete(&egressPoolMemberModel{}).Error; err != nil {
			return mapError(err)
		}
		if err := tx.Model(&egressPoolModel{}).Where("fallback_pool_id = ?", id).Updates(map[string]any{"fallback_mode": string(egress.PoolFallbackNone), "fallback_pool_id": 0}).Error; err != nil {
			return mapError(err)
		}
		if err := clearEgressRoutingPoolReferences(tx, id); err != nil {
			return err
		}
		result := tx.Delete(&egressPoolModel{}, "id = ?", id)
		if result.Error != nil {
			return mapError(result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
}

// ListEgressNodesByPool returns the members of one pool ordered by id.
// Membership is many-to-many: a node may serve several pools.
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

// EgressPoolMembers returns pool memberships as poolID → nodeIDs.
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

// egressNodePoolIDs returns nodeID → poolIDs for node listings.
func (r *EgressRepository) egressNodePoolIDs(ctx context.Context) (map[uint64][]uint64, error) {
	members, err := r.EgressPoolMembers(ctx)
	if err != nil {
		return nil, err
	}
	byNode := make(map[uint64][]uint64)
	for poolID, nodeIDs := range members {
		for _, nodeID := range nodeIDs {
			byNode[nodeID] = append(byNode[nodeID], poolID)
		}
	}
	// 分组经 Go map 迭代,顺序随机——同一节点两次列表的 PoolIDs 顺序会不同,
	// 也与 GetEgressNode 的确定性点查(pool_id ASC)不一致。排序归一为池 ID
	// 升序,两条读取路径投影一致。
	for nodeID, poolIDs := range byNode {
		sort.Slice(poolIDs, func(i, j int) bool { return poolIDs[i] < poolIDs[j] })
		byNode[nodeID] = poolIDs
	}
	return byNode, nil
}

// SetEgressPoolMembers replaces the full membership of one pool in a single
// transaction. Pool-side editing is the only membership write path.
func (r *EgressRepository) SetEgressPoolMembers(ctx context.Context, poolID uint64, nodeIDs []uint64) error {
	if poolID == 0 {
		return errors.New("pool id is required")
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 必须在 Delete 之前读取已有 priority:替换成员时保留首选设置,
		// 勾选管理不该顺手清掉星标。先删后读会恒读到空集。
		priorities := map[uint64]int64{}
		var existing []egressPoolMemberModel
		if err := tx.Where("pool_id = ?", poolID).Find(&existing).Error; err != nil {
			return mapError(err)
		}
		for _, row := range existing {
			priorities[row.NodeID] = row.Priority
		}
		if err := tx.Where("pool_id = ?", poolID).Delete(&egressPoolMemberModel{}).Error; err != nil {
			return mapError(err)
		}
		if len(nodeIDs) == 0 {
			return nil
		}
		rows := make([]egressPoolMemberModel, 0, len(nodeIDs))
		seen := make(map[uint64]struct{}, len(nodeIDs))
		for _, nodeID := range nodeIDs {
			if nodeID == 0 {
				continue
			}
			if _, exists := seen[nodeID]; exists {
				continue
			}
			seen[nodeID] = struct{}{}
			rows = append(rows, egressPoolMemberModel{PoolID: poolID, NodeID: nodeID, Priority: priorities[nodeID]})
		}
		if len(rows) > 0 {
			if err := tx.Create(&rows).Error; err != nil {
				return mapError(err)
			}
		}
		return nil
	})
}

// EgressPoolPreferredNodes 返回每池的首选节点（priority 最小者；未设置则无条目）。
func (r *EgressRepository) EgressPoolPreferredNodes(ctx context.Context) (map[uint64]uint64, error) {
	type row struct {
		PoolID   uint64
		NodeID   uint64
		Priority int64
	}
	var rows []row
	if err := r.db.db.WithContext(ctx).Model(&egressPoolMemberModel{}).
		Where("priority > 0").
		Select("pool_id, node_id, priority").Order("pool_id, priority, node_id").Scan(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	result := make(map[uint64]uint64)
	seen := make(map[uint64]bool)
	for _, item := range rows {
		if seen[item.PoolID] {
			continue
		}
		result[item.PoolID] = item.NodeID
		seen[item.PoolID] = true
	}
	return result, nil
}

// UpdateEgressPoolRotationCursor 持久化节点轮询游标，重启不归位。
func (r *EgressRepository) UpdateEgressPoolRotationCursor(ctx context.Context, poolID, fromNodeID, nodeID uint64) error {
	// CAS:仅当库中游标仍是推进前的旧值时才写,并发推进/多实例交错时旧值不会覆盖新值。
	result := r.db.db.WithContext(ctx).
		Model(&egressPoolModel{}).
		Where("id = ? AND (rotation_cursor_node_id = ? OR rotation_cursor_node_id = 0)", poolID, fromNodeID).
		Updates(map[string]any{"rotation_cursor_node_id": nodeID, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// SetEgressPoolMemberPriority 设置池内一个成员的首选顺序。priority 越小越
// 靠前；首选优先/节点轮询取“最靠前的可用成员”。
func (r *EgressRepository) SetEgressPoolMemberPriority(ctx context.Context, poolID, nodeID uint64, priority int64) error {
	result := r.db.db.WithContext(ctx).
		Model(&egressPoolMemberModel{}).
		Where("pool_id = ? AND node_id = ?", poolID, nodeID).
		Update("priority", priority)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// UpdateEgressNodeQualityState persists an exit-IP quality quarantine. It is
// a narrow targeted write so concurrent health feedback and probe updates
// cannot clobber each other.
func (r *EgressRepository) UpdateEgressNodeQualityState(ctx context.Context, id uint64, health float64, failureCount int, cooldownUntil *time.Time, lastError string, degradeCount int, lastDegradedAt *time.Time) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"health": health, "failure_count": failureCount, "cooldown_until": cooldownUntil, "last_error": lastError,
		"degrade_count": degradeCount, "last_degraded_at": lastDegradedAt, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// UpdateEgressNodeRotationState persists rotation bookkeeping without
// touching health or probe columns. lastRotatedAt==nil 表示"本次没有真正换 IP"
// (无 webhook/禁用/解密失败/尝试耗尽等跳过路径), 此时保留既有值而非写 NULL——
// 否则失败路径会抹掉上一次成功轮换的时间, 击穿 MinNodeInterval 护栏, 下一次
// 隔离事件可立即再次触发 webhook。显式传入时间才推进。
func (r *EgressRepository) UpdateEgressNodeRotationState(ctx context.Context, id uint64, lastRotatedAt *time.Time, attempts int, lastError string) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"last_rotated_at":   gorm.Expr("COALESCE(?, last_rotated_at)", lastRotatedAt),
		"rotation_attempts": attempts, "last_rotation_error": lastError, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// UpdateEgressNodeProbe persists a direct proxy probe. A healthy result also
// clears a transport-only request failure because the proxy has just been
// verified independently; anti-bot and other request failures stay intact.
func (r *EgressRepository) UpdateEgressNodeProbe(ctx context.Context, id uint64, expectedEncryptedProxyURL string, value egress.ProbeResult) error {
	updates := map[string]any{
		"probe_status": value.Status, "last_probed_at": value.TestedAt.UTC(),
		"probe_latency_ms": value.LatencyMS, "exit_ip": value.ExitIP, "probe_error": value.Error, "probe_provider": storedProbeProvider(value.Provider),
		"ipv4_probe_status": normalizedProbeStatus(value.IPv4.Status), "ipv4_last_probed_at": probeTestedAt(value.IPv4),
		"ipv4_probe_latency_ms": value.IPv4.LatencyMS, "ipv4_exit_ip": value.IPv4.ExitIP, "ipv4_probe_error": value.IPv4.Error,
		"ipv6_probe_status": normalizedProbeStatus(value.IPv6.Status), "ipv6_last_probed_at": probeTestedAt(value.IPv6),
		"ipv6_probe_latency_ms": value.IPv6.LatencyMS, "ipv6_exit_ip": value.IPv6.ExitIP, "ipv6_probe_error": value.IPv6.Error,
		"updated_at": time.Now().UTC(),
	}
	if value.Status == egress.ProbeStatusHealthy {
		condition := "last_error = ?"
		updates["health"] = gorm.Expr("CASE WHEN "+condition+" THEN ? ELSE health END", egress.LastErrorTransport, 1)
		updates["failure_count"] = gorm.Expr("CASE WHEN "+condition+" THEN ? ELSE failure_count END", egress.LastErrorTransport, 0)
		updates["cooldown_until"] = gorm.Expr("CASE WHEN "+condition+" THEN NULL ELSE cooldown_until END", egress.LastErrorTransport)
		updates["last_error"] = gorm.Expr("CASE WHEN "+condition+" THEN ? ELSE last_error END", egress.LastErrorTransport, "")
	}
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).
		Where("id = ? AND encrypted_proxy_url = ?", id, expectedEncryptedProxyURL).
		Updates(updates)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return mapError(err)
		}
		if count == 0 {
			return repository.ErrNotFound
		}
		return repository.ErrConflict
	}
	return nil
}

func (r *EgressRepository) ListDueEgressNodes(ctx context.Context, now time.Time, interval time.Duration, limit int) ([]egress.Node, error) {
	if limit < 1 {
		return []egress.Node{}, nil
	}
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	var rows []egressNodeModel
	if err := r.db.db.WithContext(ctx).
		Where("enabled = ? AND encrypted_proxy_url <> '' AND (last_probed_at IS NULL OR last_probed_at <= ?)", true, now.UTC().Add(-interval)).
		Order("last_probed_at ASC NULLS FIRST, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	values := make([]egress.Node, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressDomain(row))
	}
	return values, nil
}

func (r *EgressRepository) ListEgressSources(ctx context.Context) ([]egress.SubscriptionSource, error) {
	var rows []egressSubscriptionSourceModel
	if err := r.db.db.WithContext(ctx).Order("name ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	values := make([]egress.SubscriptionSource, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressSubscriptionSourceDomain(row))
	}
	return values, nil
}

func (r *EgressRepository) ListEgressSourcePage(ctx context.Context, input repository.EgressSourceListQuery) ([]egress.SubscriptionSource, int64, error) {
	query := r.db.db.WithContext(ctx).Model(&egressSubscriptionSourceModel{})
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		query = query.Where("LOWER(egress_subscription_sources.name) LIKE ?", "%"+strings.ToLower(search)+"%")
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, mapError(err)
	}
	var rows []egressSubscriptionSourceModel
	if err := query.Order("LOWER(egress_subscription_sources.name) ASC, egress_subscription_sources.id ASC").
		Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, mapError(err)
	}
	values := make([]egress.SubscriptionSource, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressSubscriptionSourceDomain(row))
	}
	return values, total, nil
}

func (r *EgressRepository) ListDueEgressSources(ctx context.Context, now time.Time, limit int) ([]egress.SubscriptionSource, error) {
	if limit < 1 {
		return []egress.SubscriptionSource{}, nil
	}
	var rows []egressSubscriptionSourceModel
	if err := r.db.db.WithContext(ctx).
		Where("enabled = ? AND encrypted_url <> '' AND (next_sync_at IS NULL OR next_sync_at <= ?)", true, now.UTC()).
		Order("next_sync_at ASC NULLS FIRST, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	values := make([]egress.SubscriptionSource, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressSubscriptionSourceDomain(row))
	}
	return values, nil
}

func (r *EgressRepository) GetEgressSource(ctx context.Context, id uint64) (egress.SubscriptionSource, error) {
	var row egressSubscriptionSourceModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return egress.SubscriptionSource{}, mapError(err)
	}
	return toEgressSubscriptionSourceDomain(row), nil
}

func (r *EgressRepository) CreateEgressSource(ctx context.Context, value egress.SubscriptionSource) (egress.SubscriptionSource, error) {
	row := fromEgressSubscriptionSourceDomain(value)
	if err := r.db.db.WithContext(ctx).Create(&row).Error; err != nil {
		return egress.SubscriptionSource{}, mapError(err)
	}
	return toEgressSubscriptionSourceDomain(row), nil
}

func (r *EgressRepository) UpdateEgressSource(ctx context.Context, value egress.SubscriptionSource) (egress.SubscriptionSource, error) {
	row := fromEgressSubscriptionSourceDomain(value)
	// 管理端编辑只写配置面列,与 UpdateEgressNode 修复的同类缺陷一致:此前
	// 全行 Save 会把读-改-写窗口内维护循环窄写的运行态(last_synced_at/
	// next_sync_at/last_sync_imported/last_sync_error)用陈旧快照整体回滚。
	// 调度重置(配置变更 → next_sync_at 清空、last_sync_error 清空)在 UPDATE
	// 语句内对**当前行**原子判定:仅当同步相关配置(订阅地址/拉取代理)真的
	// 变化时才重置——陈旧快照的 NextSyncAt=nil(从未同步)不再被误判为重置。
	configChanged := "(encrypted_url <> ? OR encrypted_proxy_url <> ?)"
	updates := map[string]any{
		"name": row.Name, "enabled": row.Enabled,
		"encrypted_url": row.EncryptedURL, "encrypted_proxy_url": row.EncryptedProxyURL,
		"refresh_interval_seconds": row.RefreshIntervalSeconds,
		"updated_at":              time.Now().UTC(),
		"next_sync_at": gorm.Expr("CASE WHEN "+configChanged+" THEN NULL ELSE next_sync_at END", row.EncryptedURL, row.EncryptedProxyURL),
		"last_sync_error": gorm.Expr("CASE WHEN "+configChanged+" THEN '' ELSE last_sync_error END", row.EncryptedURL, row.EncryptedProxyURL),
	}
	result := r.db.db.WithContext(ctx).Model(&egressSubscriptionSourceModel{}).Where("id = ?", value.ID).Updates(updates)
	if result.Error != nil {
		return egress.SubscriptionSource{}, mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return egress.SubscriptionSource{}, repository.ErrNotFound
	}
	// 回读合并后的真实状态(含运行态列),而不是把陈旧快照当作更新结果返回。
	return r.GetEgressSource(ctx, value.ID)
}

// DeleteEgressSource keeps already imported nodes intact. They become normal
// manually managed nodes rather than silently losing proxy configuration.
func (r *EgressRepository) DeleteEgressSource(ctx context.Context, id uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&egressNodeModel{}).Where("source_id = ?", id).Updates(map[string]any{"source_id": nil, "source_key": ""}).Error; err != nil {
			return mapError(err)
		}
		result := tx.Delete(&egressSubscriptionSourceModel{}, id)
		if result.Error != nil {
			return mapError(result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
}

func (r *EgressRepository) UpdateEgressSourceSync(ctx context.Context, id uint64, syncedAt, nextSyncAt time.Time, imported int, lastError string) error {
	result := r.db.db.WithContext(ctx).Model(&egressSubscriptionSourceModel{}).Where("id = ?", id).Updates(map[string]any{
		"last_synced_at": syncedAt.UTC(), "next_sync_at": nextSyncAt.UTC(), "last_sync_imported": imported,
		"last_sync_error": lastError, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// UpsertEgressNodesFromSource replaces the active representation of a source
// atomically. Stale nodes are disabled instead of deleted so operators keep
// their history until an explicit cleanup.
func (r *EgressRepository) UpsertEgressNodesFromSource(ctx context.Context, sourceID uint64, values []egress.Node) (int, error) {
	if sourceID == 0 {
		return 0, errors.New("subscription source id is required")
	}
	returned := 0
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existingKeys []string
		if err := tx.Model(&egressNodeModel{}).Where("source_id = ?", sourceID).Pluck("source_key", &existingKeys).Error; err != nil {
			return mapError(err)
		}
		existing := make(map[string]struct{}, len(existingKeys))
		for _, key := range existingKeys {
			existing[key] = struct{}{}
		}
		keys := make([]string, 0, len(values))
		for _, value := range values {
			if value.SourceID != sourceID || value.SourceKey == "" {
				return errors.New("invalid subscription node")
			}
			row := fromEgressDomain(value)
			// 出口地址变更时旧地址产生的观测(健康/冷却/失败/探活/ExitIP)对新地址
			// 无效——与管理端编辑 applyInput configurationChanged 的失效语义对齐。
			// CASE 对当前行判定地址是否真的变化:未变化的条目(常规重同步)完全
			// 不触运行态;变化时整组重置,但在途质量隔离(last_error=exit_ip_quality)
			// 与 UpdateEgressNode 同款守护保留。
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "source_id"}, {Name: "source_key"}},
				DoUpdates: clause.Assignments(map[string]any{
					"name": row.Name, "enabled": row.Enabled, "proxy_pool": row.ProxyPool,
					"encrypted_proxy_url": row.EncryptedProxyURL,
					"updated_at": time.Now().UTC(),
					"health": gorm.Expr("CASE WHEN egress_nodes.last_error = ? OR egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.health ELSE 1 END", egress.LastErrorExitIPQuality, row.EncryptedProxyURL),
					"failure_count": gorm.Expr("CASE WHEN egress_nodes.last_error = ? OR egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.failure_count ELSE 0 END", egress.LastErrorExitIPQuality, row.EncryptedProxyURL),
					"cooldown_until": gorm.Expr("CASE WHEN egress_nodes.last_error = ? OR egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.cooldown_until ELSE NULL END", egress.LastErrorExitIPQuality, row.EncryptedProxyURL),
					"last_error": gorm.Expr("CASE WHEN egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.last_error ELSE '' END", row.EncryptedProxyURL),
					"probe_status": gorm.Expr("CASE WHEN egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.probe_status ELSE ? END", row.EncryptedProxyURL, egress.ProbeStatusUnknown),
					"last_probed_at": gorm.Expr("CASE WHEN egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.last_probed_at ELSE NULL END", row.EncryptedProxyURL),
					"probe_latency_ms": gorm.Expr("CASE WHEN egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.probe_latency_ms ELSE 0 END", row.EncryptedProxyURL),
					"exit_ip": gorm.Expr("CASE WHEN egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.exit_ip ELSE '' END", row.EncryptedProxyURL),
					"probe_error": gorm.Expr("CASE WHEN egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.probe_error ELSE '' END", row.EncryptedProxyURL),
					"probe_provider": gorm.Expr("CASE WHEN egress_nodes.encrypted_proxy_url = ? THEN egress_nodes.probe_provider ELSE '' END", row.EncryptedProxyURL),
				}),
			}).Create(&row).Error; err != nil {
				return mapError(err)
			}
			keys = append(keys, value.SourceKey)
			if _, found := existing[value.SourceKey]; !found {
				returned++
				existing[value.SourceKey] = struct{}{}
			}
		}
		stale := tx.Model(&egressNodeModel{}).Where("source_id = ?", sourceID)
		if len(keys) > 0 {
			stale = stale.Where("source_key NOT IN ?", keys)
		}
		if err := stale.Updates(map[string]any{
			"enabled": false, "probe_status": string(egress.ProbeStatusUnknown), "probe_error": "subscription entry removed", "probe_provider": "",
			"ipv4_probe_status": string(egress.ProbeStatusUnknown), "ipv4_last_probed_at": nil, "ipv4_probe_latency_ms": 0, "ipv4_exit_ip": "", "ipv4_probe_error": "subscription entry removed",
			"ipv6_probe_status": string(egress.ProbeStatusUnknown), "ipv6_last_probed_at": nil, "ipv6_probe_latency_ms": 0, "ipv6_exit_ip": "", "ipv6_probe_error": "subscription entry removed",
			"updated_at": time.Now().UTC(),
		}).Error; err != nil {
			return mapError(err)
		}
		return clearInvalidEgressRoutingReferences(tx)
	})
	return returned, err
}

func (r *EgressRepository) GetEgressOperationsConfig(ctx context.Context) (egress.OperationsConfig, error) {
	var row egressOperationsConfigModel
	if err := r.db.db.WithContext(ctx).First(&row, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return egress.DefaultOperationsConfig(), nil
		}
		return egress.OperationsConfig{}, mapError(err)
	}
	config, err := toEgressOperationsConfigDomain(row)
	if err != nil {
		return egress.OperationsConfig{}, err
	}
	return config, nil
}

func (r *EgressRepository) SaveEgressOperationsConfig(ctx context.Context, value egress.OperationsConfig) (egress.OperationsConfig, error) {
	row := fromEgressOperationsConfigDomain(value)
	row.ID = 1
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockEgressOperationsConfig(tx); err != nil {
			return err
		}
		if err := validateLockedEgressRouting(tx, row); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
	})
	if err != nil {
		return egress.OperationsConfig{}, mapError(err)
	}
	config, err := toEgressOperationsConfigDomain(row)
	if err != nil {
		return egress.OperationsConfig{}, mapError(err)
	}
	return config, nil
}

// SaveEgressOperationsConfigIfCurrent 条件写入运营配置:仅在行锁事务内
// 确认配置自 since(调用方快照的 UpdatedAt)以来未被其他写入者修改时才
// 落库;已变化时返回 repository.ErrEgressConfigStale,由调用方重读重算。
// 供订阅同步后的路由卫生检查这类后台写者使用,防止其旧快照整行覆盖并发
// 的管理员提交;管理员路径继续用无条件 SaveEgressOperationsConfig。
//
// since 必须取自 GetEgressOperationsConfig 的读取结果:SaveEgressOperations
// Config 的返回值是内存构造行,其 UpdatedAt 未经存储往返(驱动可能截断
// 精度),不能直接用作条件写令牌。
func (r *EgressRepository) SaveEgressOperationsConfigIfCurrent(ctx context.Context, value egress.OperationsConfig, since time.Time) (egress.OperationsConfig, error) {
	row := fromEgressOperationsConfigDomain(value)
	row.ID = 1
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := lockEgressOperationsConfig(tx)
		if err != nil {
			return err
		}
		if !current.UpdatedAt.Equal(since) {
			return repository.ErrEgressConfigStale
		}
		if err := validateLockedEgressRouting(tx, row); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
	})
	if err != nil {
		return egress.OperationsConfig{}, mapError(err)
	}
	config, err := toEgressOperationsConfigDomain(row)
	if err != nil {
		return egress.OperationsConfig{}, mapError(err)
	}
	return config, nil
}

func lockEgressOperationsConfig(tx *gorm.DB) (egressOperationsConfigModel, error) {
	defaults := fromEgressOperationsConfigDomain(egress.DefaultOperationsConfig())
	defaults.ID = 1
	defaults.UpdatedAt = time.Now().UTC()
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&defaults).Error; err != nil {
		return egressOperationsConfigModel{}, err
	}
	var row egressOperationsConfigModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, 1).Error; err != nil {
		return egressOperationsConfigModel{}, err
	}
	return row, nil
}

// routingTargetRow converts a domain routing target to its persisted row.
func routingTargetRow(target egress.RoutingTarget) egressRoutingTargetRow {
	return egressRoutingTargetRow{Mode: string(target.Mode.Normalized()), NodeID: target.NodeID, PoolID: target.PoolID}
}

func routingTargetFromRow(row egressRoutingTargetRow) egress.RoutingTarget {
	return egress.RoutingTarget{Mode: egress.RoutingTargetMode(row.Mode).Normalized(), NodeID: row.NodeID, PoolID: row.PoolID}
}

// configReferencesAnyRoutingNode reports whether any configured node target
// (default, scope, or class level) references one of the supplied ids.
func configReferencesAnyRoutingNode(encoded string, ids []uint64) bool {
	if len(ids) == 0 || strings.TrimSpace(encoded) == "" {
		return false
	}
	payload, err := unmarshalEgressRouting(encoded)
	if err != nil {
		return false
	}
	selected := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		selected[id] = struct{}{}
	}
	if payload.Default != nil && payload.Default.NodeID != 0 {
		if _, exists := selected[payload.Default.NodeID]; exists {
			return true
		}
	}
	for _, target := range payload.Scopes {
		if target.NodeID == 0 {
			continue
		}
		if _, exists := selected[target.NodeID]; exists {
			return true
		}
	}
	for _, target := range payload.Classes {
		if target.NodeID == 0 {
			continue
		}
		if _, exists := selected[target.NodeID]; exists {
			return true
		}
	}
	return false
}

// validateLockedEgressRouting verifies that every configured node target is
// schedulable and every pool target exists, so a saved routing decision can
// never silently degrade to the automatic schedule.
func validateLockedEgressRouting(tx *gorm.DB, config egressOperationsConfigModel) error {
	payload, err := unmarshalEgressRouting(config.Routing)
	if err != nil {
		return repository.ErrEgressRoutingInvalid
	}
	nodeIDs := make([]uint64, 0)
	poolIDs := make([]uint64, 0)
	if payload.Default != nil {
		if target := routingTargetFromRow(*payload.Default); target.NodeID != 0 {
			nodeIDs = append(nodeIDs, target.NodeID)
		} else if target.PoolID != 0 {
			poolIDs = append(poolIDs, target.PoolID)
		}
	}
	for _, row := range payload.Scopes {
		if target := routingTargetFromRow(row); target.NodeID != 0 {
			nodeIDs = append(nodeIDs, target.NodeID)
		} else if target.PoolID != 0 {
			poolIDs = append(poolIDs, target.PoolID)
		}
	}
	for _, row := range payload.Classes {
		if target := routingTargetFromRow(row); target.NodeID != 0 {
			nodeIDs = append(nodeIDs, target.NodeID)
		} else if target.PoolID != 0 {
			poolIDs = append(poolIDs, target.PoolID)
		}
	}
	if len(nodeIDs) > 0 {
		var nodes []egressNodeModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", nodeIDs).Order("id ASC").Find(&nodes).Error; err != nil {
			return err
		}
		byID := make(map[uint64]egressNodeModel, len(nodes))
		for _, node := range nodes {
			byID[node.ID] = node
		}
		for _, id := range nodeIDs {
			node, exists := byID[id]
			if !exists || !egress.CanNodeServeFixedTarget(egress.Node{ID: node.ID, Enabled: node.Enabled, ProxyPool: node.ProxyPool, EncryptedProxyURL: node.EncryptedProxyURL}) {
				return repository.ErrEgressRoutingNodeInUse
			}
		}
	}
	if len(poolIDs) > 0 {
		var count int64
		if err := tx.Model(&egressPoolModel{}).Where("id IN ?", poolIDs).Count(&count).Error; err != nil {
			return err
		}
		if count != int64(len(uniqueUint64(poolIDs))) {
			return repository.ErrEgressRoutingInvalid
		}
	}
	return nil
}

func uniqueUint64(values []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(values))
	result := make([]uint64, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// clearEgressRoutingNodeReferences removes configured node targets that
// reference any of the supplied node ids, letting each level fall through
// to the next (scope → default → automatic schedule).
func clearEgressRoutingNodeReferences(tx *gorm.DB, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	var config egressOperationsConfigModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&config, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	payload, err := unmarshalEgressRouting(config.Routing)
	if err != nil || (payload.Default == nil && len(payload.Scopes) == 0 && len(payload.Classes) == 0) {
		return nil
	}
	selected := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		selected[id] = struct{}{}
	}
	changed := false
	if payload.Default != nil && payload.Default.NodeID != 0 {
		if _, exists := selected[payload.Default.NodeID]; exists {
			payload.Default = nil
			changed = true
		}
	}
	for key, target := range payload.Scopes {
		if target.NodeID != 0 {
			if _, exists := selected[target.NodeID]; exists {
				delete(payload.Scopes, key)
				changed = true
			}
		}
	}
	for key, target := range payload.Classes {
		if target.NodeID != 0 {
			if _, exists := selected[target.NodeID]; exists {
				delete(payload.Classes, key)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return tx.Model(&egressOperationsConfigModel{}).Where("id = ?", 1).
		Updates(map[string]any{"routing": marshalEgressRouting(payload), "updated_at": time.Now().UTC()}).Error
}

// clearEgressRoutingPoolReferences removes configured pool targets that
// reference the supplied pool id (pool deletion).
func clearEgressRoutingPoolReferences(tx *gorm.DB, poolID uint64) error {
	var config egressOperationsConfigModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&config, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	payload, err := unmarshalEgressRouting(config.Routing)
	if err != nil {
		return nil
	}
	changed := false
	if payload.Default != nil && payload.Default.PoolID == poolID {
		payload.Default = nil
		changed = true
	}
	for key, target := range payload.Scopes {
		if target.PoolID == poolID {
			delete(payload.Scopes, key)
			changed = true
		}
	}
	for key, target := range payload.Classes {
		if target.PoolID == poolID {
			delete(payload.Classes, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return tx.Model(&egressOperationsConfigModel{}).Where("id = ?", 1).
		Updates(map[string]any{"routing": marshalEgressRouting(payload), "updated_at": time.Now().UTC()}).Error
}

// clearInvalidEgressRoutingReferences drops targets whose node no longer
// satisfies the schedulable contract or whose pool vanished, mirroring the
// save-time validation after subscription node replacement.
func clearInvalidEgressRoutingReferences(tx *gorm.DB) error {
	var config egressOperationsConfigModel
	// Lock like the delete path: an unlocked read could interleave with a
	// concurrent Save and lose freshly written targets (lost update).
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&config, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	payload, err := unmarshalEgressRouting(config.Routing)
	if err != nil || (payload.Default == nil && len(payload.Scopes) == 0 && len(payload.Classes) == 0) {
		return nil
	}
	nodeIDs := make([]uint64, 0)
	collect := func(target egressRoutingTargetRow) {
		if target.NodeID != 0 {
			nodeIDs = append(nodeIDs, target.NodeID)
		}
	}
	if payload.Default != nil {
		collect(*payload.Default)
	}
	for _, target := range payload.Scopes {
		collect(target)
	}
	for _, target := range payload.Classes {
		collect(target)
	}
	validNodes := make(map[uint64]bool)
	if len(nodeIDs) > 0 {
		var nodes []egressNodeModel
		if err := tx.Where("id IN ?", uniqueUint64(nodeIDs)).Find(&nodes).Error; err != nil {
			return err
		}
		for _, node := range nodes {
			validNodes[node.ID] = egress.CanNodeServeFixedTarget(egress.Node{ID: node.ID, Enabled: node.Enabled, ProxyPool: node.ProxyPool, EncryptedProxyURL: node.EncryptedProxyURL})
		}
	}
	validPools := make(map[uint64]bool)
	poolIDs := make([]uint64, 0)
	if payload.Default != nil && payload.Default.PoolID != 0 {
		poolIDs = append(poolIDs, payload.Default.PoolID)
	}
	for _, target := range payload.Scopes {
		if target.PoolID != 0 {
			poolIDs = append(poolIDs, target.PoolID)
		}
	}
	for _, target := range payload.Classes {
		if target.PoolID != 0 {
			poolIDs = append(poolIDs, target.PoolID)
		}
	}
	if len(poolIDs) > 0 {
		var pools []egressPoolModel
		if err := tx.Where("id IN ?", uniqueUint64(poolIDs)).Find(&pools).Error; err != nil {
			return err
		}
		for _, pool := range pools {
			validPools[pool.ID] = pool.Enabled
		}
	}
	targetValid := func(target egressRoutingTargetRow) bool {
		switch egress.RoutingTargetMode(target.Mode).Normalized() {
		case egress.RoutingTargetNode:
			return validNodes[target.NodeID]
		case egress.RoutingTargetPool:
			return validPools[target.PoolID]
		default:
			return true
		}
	}
	changed := false
	if payload.Default != nil && !targetValid(*payload.Default) {
		payload.Default = nil
		changed = true
	}
	for key, target := range payload.Scopes {
		if !targetValid(target) {
			delete(payload.Scopes, key)
			changed = true
		}
	}
	for key, target := range payload.Classes {
		if !targetValid(target) {
			delete(payload.Classes, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return tx.Model(&egressOperationsConfigModel{}).Where("id = ?", 1).
		Updates(map[string]any{"routing": marshalEgressRouting(payload), "updated_at": time.Now().UTC()}).Error
}

func (r *EgressRepository) DeleteEgressNode(ctx context.Context, id uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := clearEgressRoutingNodeReferences(tx, []uint64{id}); err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", id).Delete(&egressPoolMemberModel{}).Error; err != nil {
			return err
		}
		result := tx.Delete(&egressNodeModel{}, id)
		if result.Error != nil {
			return mapError(result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
}

// DeleteEgressNodes deletes a selection atomically.
func (r *EgressRepository) DeleteEgressNodes(ctx context.Context, ids []uint64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		deleted, err = deleteEgressNodeIDs(tx, ids)
		return err
	})
	return int(deleted), mapError(err)
}

func (r *EgressRepository) PreviewUnhealthyEgressNodes(ctx context.Context) (repository.EgressNodeCleanupPreview, error) {
	var result repository.EgressNodeCleanupPreview
	if err := r.unhealthyEgressNodes(ctx).Count(&result.Nodes).Error; err != nil {
		return result, mapError(err)
	}
	if err := r.unhealthyEgressNodes(ctx).Where("source_id IS NOT NULL").Count(&result.SubscriptionManaged).Error; err != nil {
		return result, mapError(err)
	}
	return result, nil
}

func (r *EgressRepository) unhealthyEgressNodes(ctx context.Context) *gorm.DB {
	return r.db.db.WithContext(ctx).Model(&egressNodeModel{}).
		Where("ipv4_probe_status = ? AND ipv6_probe_status = ?", egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy)
}

func (r *EgressRepository) DeleteUnhealthyEgressNodes(ctx context.Context) ([]uint64, error) {
	ids := make([]uint64, 0)
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&egressNodeModel{}).
			Where("ipv4_probe_status = ? AND ipv6_probe_status = ?", egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy).
			Order("id ASC")
		if tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.Pluck("id", &ids).Error; err != nil {
			return err
		}
		_, err := deleteEgressNodeIDs(tx, ids)
		return err
	})
	return ids, mapError(err)
}

func deleteEgressNodeIDs(tx *gorm.DB, ids []uint64) (int64, error) {
	var deleted int64
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		batch := ids[start:end]
		if err := clearEgressRoutingNodeReferences(tx, batch); err != nil {
			return deleted, err
		}
		if result := tx.Where("node_id IN ?", batch).Delete(&egressPoolMemberModel{}); result.Error != nil {
			return deleted, result.Error
		}
		result := tx.Where("id IN ?", batch).Delete(&egressNodeModel{})
		if result.Error != nil {
			return deleted, result.Error
		}
		deleted += result.RowsAffected
	}
	return deleted, nil
}

// egressSourceNames loads the subscription-name lookup for node listings.
func (r *EgressRepository) egressSourceNames(ctx context.Context) (map[uint64]string, error) {
	var rows []egressSubscriptionSourceModel
	if err := r.db.db.WithContext(ctx).Select("id", "name").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	names := make(map[uint64]string, len(rows))
	for _, row := range rows {
		names[row.ID] = row.Name
	}
	return names, nil
}

func toEgressDomain(row egressNodeModel) egress.Node {
	return egress.Node{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled, ProxyPool: row.ProxyPool,
		SourceID: valueEgressNodeID(row.SourceID), SourceKey: row.SourceKey,
		EncryptedProxyURL: row.EncryptedProxyURL, UserAgent: row.UserAgent, EncryptedCloudflareCookie: row.EncryptedCloudflareCookie,
		ClearanceRefreshedAt: row.ClearanceRefreshedAt, ClearanceFingerprint: row.ClearanceFingerprint,
		ClearanceBindingFingerprint: row.ClearanceBindingFingerprint,
		EncryptedRotationURL:        row.EncryptedRotationURL, RotationEnabled: row.RotationEnabled, LastRotatedAt: row.LastRotatedAt,
		RotationAttempts: row.RotationAttempts, LastRotationError: row.LastRotationError,
		DegradeCount: row.DegradeCount, LastDegradedAt: row.LastDegradedAt,
		Health: row.Health, FailureCount: row.FailureCount, CooldownUntil: row.CooldownUntil, LastError: row.LastError,
		ProbeStatus: egress.ProbeStatus(row.ProbeStatus), LastProbedAt: row.LastProbedAt, ProbeLatencyMS: row.ProbeLatencyMS, ExitIP: row.ExitIP, ProbeError: row.ProbeError,
		ProbeProvider: storedProbeProvider(egress.ProbeProvider(row.ProbeProvider)),
		IPv4Probe:     probeFamilyFromRow(row.IPv4ProbeStatus, row.IPv4LastProbedAt, row.IPv4ProbeLatencyMS, row.IPv4ExitIP, row.IPv4ProbeError),
		IPv6Probe:     probeFamilyFromRow(row.IPv6ProbeStatus, row.IPv6LastProbedAt, row.IPv6ProbeLatencyMS, row.IPv6ExitIP, row.IPv6ProbeError),
		CreatedAt:     row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func fromEgressDomain(value egress.Node) egressNodeModel {
	health := value.Health
	if health == 0 && value.ID == 0 {
		health = 1
	}
	probeStatus := value.ProbeStatus
	if !probeStatus.IsValid() {
		probeStatus = egress.ProbeStatusUnknown
	}
	return egressNodeModel{
		ID: value.ID, Name: value.Name, Enabled: value.Enabled, ProxyPool: value.ProxyPool,
		SourceID: egressNodeID(value.SourceID), SourceKey: value.SourceKey,
		EncryptedProxyURL: value.EncryptedProxyURL, UserAgent: value.UserAgent, EncryptedCloudflareCookie: value.EncryptedCloudflareCookie,
		ClearanceRefreshedAt: value.ClearanceRefreshedAt, ClearanceFingerprint: value.ClearanceFingerprint,
		ClearanceBindingFingerprint: value.ClearanceBindingFingerprint,
		EncryptedRotationURL:        value.EncryptedRotationURL, RotationEnabled: value.RotationEnabled, LastRotatedAt: value.LastRotatedAt,
		RotationAttempts: value.RotationAttempts, LastRotationError: value.LastRotationError,
		DegradeCount: value.DegradeCount, LastDegradedAt: value.LastDegradedAt,
		Health: health, FailureCount: value.FailureCount, CooldownUntil: value.CooldownUntil, LastError: value.LastError,
		ProbeStatus: string(probeStatus), LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		ProbeProvider:   string(storedProbeProvider(value.ProbeProvider)),
		IPv4ProbeStatus: string(normalizedProbeStatus(value.IPv4Probe.Status)), IPv4LastProbedAt: probeTestedAt(value.IPv4Probe), IPv4ProbeLatencyMS: value.IPv4Probe.LatencyMS, IPv4ExitIP: value.IPv4Probe.ExitIP, IPv4ProbeError: value.IPv4Probe.Error,
		IPv6ProbeStatus: string(normalizedProbeStatus(value.IPv6Probe.Status)), IPv6LastProbedAt: probeTestedAt(value.IPv6Probe), IPv6ProbeLatencyMS: value.IPv6Probe.LatencyMS, IPv6ExitIP: value.IPv6Probe.ExitIP, IPv6ProbeError: value.IPv6Probe.Error,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func normalizedProbeStatus(value egress.ProbeStatus) egress.ProbeStatus {
	if value.IsValid() {
		return value
	}
	return egress.ProbeStatusUnknown
}

func storedProbeProvider(value egress.ProbeProvider) egress.ProbeProvider {
	if value.IsValid() {
		return value
	}
	return ""
}

func probeTestedAt(value egress.ProbeFamilyResult) *time.Time {
	if value.TestedAt.IsZero() {
		return nil
	}
	testedAt := value.TestedAt.UTC()
	return &testedAt
}

func probeFamilyFromRow(status string, testedAt *time.Time, latencyMS int, exitIP, probeError string) egress.ProbeFamilyResult {
	result := egress.ProbeFamilyResult{
		Status: normalizedProbeStatus(egress.ProbeStatus(status)), LatencyMS: latencyMS, ExitIP: exitIP, Error: probeError,
	}
	if testedAt != nil {
		result.TestedAt = testedAt.UTC()
	}
	return result
}

func toEgressSubscriptionSourceDomain(row egressSubscriptionSourceModel) egress.SubscriptionSource {
	return egress.SubscriptionSource{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled, EncryptedURL: row.EncryptedURL, EncryptedProxyURL: row.EncryptedProxyURL,
		RefreshIntervalSeconds: row.RefreshIntervalSeconds,
		LastSyncedAt:           row.LastSyncedAt, NextSyncAt: row.NextSyncAt, LastSyncImported: row.LastSyncImported, LastSyncError: row.LastSyncError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func fromEgressSubscriptionSourceDomain(value egress.SubscriptionSource) egressSubscriptionSourceModel {
	return egressSubscriptionSourceModel{
		ID: value.ID, Name: value.Name, Enabled: value.Enabled, EncryptedURL: value.EncryptedURL, EncryptedProxyURL: value.EncryptedProxyURL,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds,
		LastSyncedAt:           value.LastSyncedAt, NextSyncAt: value.NextSyncAt, LastSyncImported: value.LastSyncImported, LastSyncError: value.LastSyncError,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func toEgressOperationsConfigDomain(row egressOperationsConfigModel) (egress.OperationsConfig, error) {
	// A corrupted routing payload (for example a manual SQL edit) must degrade
	// to the automatic schedule so probing and administrator repair keep
	// working; failing the whole read would lock the operations config.
	payload, err := unmarshalEgressRouting(row.Routing)
	if err != nil {
		slog.Warn("egress routing payload is corrupt; falling back to automatic schedule", "error", err)
		payload = egressRoutingPayload{}
	}
	config := egress.OperationsConfig{
		ProbeProvider:        egress.ProbeProvider(row.ProbeProvider).Normalized(),
		ProbeIntervalSeconds: row.ProbeIntervalSeconds,
		UpdatedAt:            row.UpdatedAt,
	}
	if payload.Default != nil {
		config.DefaultTarget = routingTargetFromRow(*payload.Default)
	}
	if len(payload.Scopes) > 0 {
		config.ScopeTargets = make(map[egress.Scope]egress.RoutingTarget, len(payload.Scopes))
		for key, target := range payload.Scopes {
			config.ScopeTargets[egress.Scope(key)] = routingTargetFromRow(target)
		}
	}
	if len(payload.Classes) > 0 {
		config.ClassTargets = make(map[egress.TrafficClass]egress.RoutingTarget, len(payload.Classes))
		for key, target := range payload.Classes {
			config.ClassTargets[egress.TrafficClass(key)] = routingTargetFromRow(target)
		}
	}
	return config, nil
}

func fromEgressOperationsConfigDomain(value egress.OperationsConfig) egressOperationsConfigModel {
	payload := egressRoutingPayload{}
	if value.DefaultTarget.Configured() {
		row := routingTargetRow(value.DefaultTarget)
		payload.Default = &row
	}
	if len(value.ScopeTargets) > 0 {
		payload.Scopes = make(map[string]egressRoutingTargetRow, len(value.ScopeTargets))
		for scope, target := range value.ScopeTargets {
			payload.Scopes[string(scope)] = routingTargetRow(target)
		}
	}
	if len(value.ClassTargets) > 0 {
		payload.Classes = make(map[string]egressRoutingTargetRow, len(value.ClassTargets))
		for class, target := range value.ClassTargets {
			payload.Classes[string(class)] = routingTargetRow(target)
		}
	}
	return egressOperationsConfigModel{
		ID: 1, ProbeProvider: string(value.ProbeProvider.Normalized()), ProbeIntervalSeconds: value.ProbeIntervalSeconds,
		Routing:   marshalEgressRouting(payload),
		UpdatedAt: value.UpdatedAt,
	}
}
