package registry

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
)

// qIncidentClosureModel is a point-lookup projection of the latest closure.
// It is not loaded into state snapshots, and survives removal of old case
// details so delayed observations cannot resurrect an already settled event.
type qIncidentClosureModel struct {
	AccountID uint64    `gorm:"primaryKey;autoIncrement:false"`
	NodeID    uint64    `gorm:"primaryKey;autoIncrement:false"`
	Epoch     uint64    `gorm:"primaryKey;autoIncrement:false"`
	ClosedAt  time.Time `gorm:"not null"`
}

func (qIncidentClosureModel) TableName() string { return "q_incident_closure" }

const incidentClosureSelect = `SELECT a.account_id, COALESCE(e.node_id,0), COALESCE(e.epoch,0), MAX(c.closed_at)
	FROM q_case_party a JOIN q_case c ON c.id=a.case_id
	LEFT JOIN q_case_party e ON e.case_id=a.case_id AND e.kind='exit'
	WHERE a.kind='account' AND a.account_id<>0 AND c.closed_at IS NOT NULL`

func recordIncidentClosures(tx *gorm.DB, caseID *uint64) error {
	query := `INSERT INTO q_incident_closure (account_id,node_id,epoch,closed_at) ` + incidentClosureSelect
	var args []any
	if caseID != nil {
		query += " AND c.id=?"
		args = append(args, *caseID)
	}
	query += ` GROUP BY a.account_id,e.node_id,e.epoch
		ON CONFLICT (account_id,node_id,epoch) DO UPDATE SET closed_at=excluded.closed_at
		WHERE excluded.closed_at > q_incident_closure.closed_at`
	return tx.Exec(query, args...).Error
}

func (r *Registry) migrateIncidentClosures(ctx context.Context) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		claim := tx.Model(&qStateRevisionModel{}).Where("id=1 AND incidents_initialized=?", false).
			Update("incidents_initialized", true)
		if claim.Error != nil || claim.RowsAffected == 0 {
			return claim.Error
		}
		return recordIncidentClosures(tx, nil)
	})
}

// LastClosedAtForIncidents fetches only requested keys. Batches bound SQL size
// and allocations independently of the retained case/epoch history.
func (r *Registry) LastClosedAtForIncidents(ctx context.Context, incidents []model.IncidentKey) (map[model.IncidentKey]time.Time, error) {
	out := make(map[model.IncidentKey]time.Time)
	seen := make(map[model.IncidentKey]bool, len(incidents))
	keys := make([]model.IncidentKey, 0, len(incidents))
	for _, key := range incidents {
		if key.AccountID != 0 && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	const batchSize = 100
	for start := 0; start < len(keys); start += batchSize {
		query := r.db.WithContext(ctx).Model(&qIncidentClosureModel{})
		for i, key := range keys[start:min(start+batchSize, len(keys))] {
			if i == 0 {
				query = query.Where("account_id=? AND node_id=? AND epoch=?", key.AccountID, key.Exit.NodeID, key.Exit.Epoch)
			} else {
				query = query.Or("account_id=? AND node_id=? AND epoch=?", key.AccountID, key.Exit.NodeID, key.Exit.Epoch)
			}
		}
		var rows []qIncidentClosureModel
		if err := query.Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[model.IncidentKey{AccountID: row.AccountID, Exit: model.EpochKey{NodeID: row.NodeID, Epoch: row.Epoch}}] = row.ClosedAt
		}
	}
	return out, nil
}
