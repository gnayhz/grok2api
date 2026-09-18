package registry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
)

// No case, account state or incident closure is written by a manual check.
func (s *ProbeTaskStore) CreateAccountCheck(ctx context.Context, task model.ProbeTask, capacity int) (uint64, error) {
	if task.Direction != model.ProbeAccountCheck || task.CaseID != 0 || task.DefendantAccountID == 0 || task.Experiment.Version != model.AccountCheckVersion || task.Experiment.UnsupportedReason() != "" || capacity < 1 {
		return 0, errors.New("invalid account check")
	}
	ctx, release, err := s.registry.Coordinate(ctx, "account_checks")
	if err != nil {
		return 0, err
	}
	defer release()
	var id uint64
	err = s.registry.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkCoordination(tx, ctx); err != nil {
			return err
		}
		active := tx.Model(&qProbeTaskModel{}).Where("direction IN ? AND state IN ?", manualProbeDirections, []string{"pending", "running"})
		var existing []qProbeTaskModel
		if err := active.Session(&gorm.Session{}).Where("direction = ? AND defendant_account_id = ?", string(model.ProbeAccountCheck), task.DefendantAccountID).Order("id DESC").Limit(1).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) > 0 {
			id = existing[0].ID
			return nil
		}
		var count int64
		if err := active.Count(&count).Error; err != nil {
			return err
		}
		if count >= int64(capacity) {
			return model.ErrCheckQueueFull
		}
		store := &ProbeTaskStore{registry: &Registry{db: tx, inTransition: true}, owner: s.owner}
		var err error
		id, err = store.CreateProbeTask(ctx, task)
		return err
	})
	return id, err
}

func (s *ProbeTaskStore) ListAccountChecks(ctx context.Context, accountID uint64) ([]model.AccountCheck, error) {
	var rows []qProbeTaskModel
	if err := s.registry.db.WithContext(ctx).Where("direction = ? AND defendant_account_id = ?", string(model.ProbeAccountCheck), accountID).Order("id DESC").Limit(20).Find(&rows).Error; err != nil {
		return nil, err
	}
	checks := make([]model.AccountCheck, 0, len(rows))
	for _, row := range rows {
		check := model.AccountCheck{ID: row.ID, AccountID: row.DefendantAccountID, Model: probeExperimentFromJSON(row.ExperimentJSON).Baseline.Model, State: model.ProbeTaskState(row.State), CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt}
		var report model.AccountCheckReport
		if json.Unmarshal([]byte(row.CheckReportJSON), &report) == nil && report.Version == model.AccountCheckVersion {
			check.Report = &report
		}
		checks = append(checks, check)
	}
	return checks, nil
}

func (r *Registry) expirePendingAccountChecks(ctx context.Context) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Model(&qProbeTaskModel{}).Where("state = ?", "pending").Where("(direction = ? AND created_at < ?) OR (direction = ? AND created_at < ?)", string(model.ProbeAccountCheck), now.Add(-10*time.Minute), string(model.ProbeResourceCheck), now.Add(-model.ResourceCheckQueueTimeout)).Updates(map[string]any{"state": "cancelled", "detail": "resource check queue deadline", "finished_at": now, "updated_at": now}).Error
}
