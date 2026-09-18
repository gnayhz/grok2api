package registry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
)

// Independent from task retention: an acknowledged result and its projection
// intent commit together; pending projection payloads survive case deletion.
type qProbeProjectionModel struct {
	TaskID   uint64    `gorm:"primaryKey"`
	Payload  string    `gorm:"type:text;not null"`
	ReadyAt  time.Time `gorm:"not null;index"`
	Attempts int       `gorm:"not null;default:0"`
}

func (qProbeProjectionModel) TableName() string { return "q_probe_projection" }

// ProcessProbeProjections is safe across instances: Record must deduplicate the
// stable Observation.EventID. A crash between Record and deletion only replays
// that same fact. Each failure backs off without blocking unrelated projections.
func (s *ProbeTaskStore) ProcessProbeProjections(ctx context.Context, limit int, record func(context.Context, model.Observation) error) (int, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if _, err := s.BackfillProbeProjections(ctx, limit); err != nil {
		return 0, err
	}
	var rows []qProbeProjectionModel
	if err := s.registry.db.WithContext(ctx).Where("ready_at <= ?", time.Now().UTC()).Order("ready_at, task_id").Limit(limit).Find(&rows).Error; err != nil {
		return 0, err
	}
	var errs []error
	for _, row := range rows {
		var obs model.Observation
		err := json.Unmarshal([]byte(row.Payload), &obs)
		if err == nil {
			err = record(ctx, obs)
		}
		if err != nil {
			errs = append(errs, err)
			backoff := time.Second * time.Duration(1<<min(row.Attempts, 6))
			if updateErr := s.registry.db.WithContext(ctx).Model(&qProbeProjectionModel{}).Where("task_id = ?", row.TaskID).
				Updates(map[string]any{"ready_at": time.Now().UTC().Add(backoff), "attempts": gorm.Expr("attempts + 1")}).Error; updateErr != nil {
				errs = append(errs, updateErr)
			}
		} else if err := s.registry.db.WithContext(ctx).Where("task_id = ?", row.TaskID).Delete(&qProbeProjectionModel{}).Error; err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return len(rows), errors.Join(errs...)
}

// BackfillProbeProjections repairs pre-upgrade done rows without re-executing
// their experiments. Legacy observations are matched before adding a stable-ID
// projection so upgrading does not manufacture duplicate historical samples.
func (s *ProbeTaskStore) BackfillProbeProjections(ctx context.Context, limit int) (int, error) {
	var rows []qProbeTaskModel
	if err := s.registry.db.WithContext(ctx).Where("state = ? AND projection_version = 0 AND direction NOT IN ?", "done", manualProbeDirections).Order("id").Limit(limit).Find(&rows).Error; err != nil {
		return 0, err
	}
	for _, row := range rows {
		err := s.registry.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			res := tx.Model(&qProbeTaskModel{}).Where("id = ? AND projection_version = 0", row.ID).Update("projection_version", 1)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 || row.FinishedAt == nil {
				return nil
			}
			result := model.ProbeTaskResult{Attempt: probeAttemptIdentity(row.AttemptJSON), Outcome: model.ProbeResult(row.Result), Detail: row.Detail}
			obs := model.ProbeObservation(probeTaskFromRow(row), result, *row.FinishedAt)
			var count int64
			query := tx.Model(&evidence.ObservationModel{}).Where("source = ?", "probe")
			if obs.Attempt.ID != "" {
				query = query.Where("attempt_id = ?", obs.Attempt.ID)
			} else {
				query = query.Where("account_id = ? AND node_id = ? AND epoch = ? AND at = ?", obs.AccountID, obs.Exit.NodeID, obs.Exit.Epoch, obs.At)
			}
			if err := query.Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return nil
			}
			payload, err := json.Marshal(obs)
			if err != nil {
				return err
			}
			return tx.Create(&qProbeProjectionModel{TaskID: row.ID, Payload: string(payload), ReadyAt: time.Now().UTC()}).Error
		})
		if err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
