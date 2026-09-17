package registry

import (
	"context"
	"time"

	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 节点降智台账(G8):流行病学管理数据,永久保留(B3 决议3),
// 不影响调度资格。节点看总数,历史看 IP 明细。

// AppendDegrade 记录一次降智事件到台账:同键计数递增,新键插入。
// 台账与状态转移无关(写台账永不改变调度资格)。
func (r *Registry) AppendDegrade(ctx context.Context, nodeID, epoch uint64, ip string, at time.Time) error {
	if nodeID == 0 {
		return ErrInvalidNode
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	row := qDegradeLedgerModel{
		NodeID:  nodeID,
		Epoch:   epoch,
		IP:      ip,
		Count:   1,
		FirstAt: at.UTC(),
		LastAt:  at.UTC(),
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}, {Name: "epoch"}, {Name: "ip"}},
		DoUpdates: clause.Assignments(map[string]any{
			"count":   gorm.Expr("count + 1"),
			"last_at": at.UTC(),
		}),
	}).Create(&row).Error
}

// NodeDegradeTotal 返回节点历史累计降智次数(全部 epoch)。
func (r *Registry) NodeDegradeTotal(ctx context.Context, nodeID uint64) (int64, error) {
	var total int64
	err := r.db.WithContext(ctx).Model(&qDegradeLedgerModel{}).
		Where("node_id = ?", nodeID).
		Select("COALESCE(SUM(count), 0)").Scan(&total).Error
	return total, err
}

// ListNodeDegradeHistory 返回节点台账明细(按最近时间倒序)。
func (r *Registry) ListNodeDegradeHistory(ctx context.Context, nodeID uint64, limit int) ([]qualitymodel.DegradeLedgerEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []qDegradeLedgerModel
	if err := r.db.WithContext(ctx).Where("node_id = ?", nodeID).
		Order("last_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]qualitymodel.DegradeLedgerEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, qualitymodel.DegradeLedgerEntry{
			NodeID: row.NodeID, Epoch: row.Epoch, IP: row.IP,
			Count: row.Count, FirstAt: row.FirstAt, LastAt: row.LastAt,
		})
	}
	return entries, nil
}
