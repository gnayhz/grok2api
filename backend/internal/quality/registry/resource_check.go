package registry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
)

var manualProbeDirections = []string{string(model.ProbeAccountCheck), string(model.ProbeResourceCheck)}

// The generation owner has two shared measurement slots. A batch must wait in
// the durable queue instead of having eight workers race for those two slots.
func (s *ProbeTaskStore) claimResourceCheck(ctx context.Context, row qProbeTaskModel, now time.Time) (bool, error) {
	ctx, release, err := s.registry.Coordinate(ctx, "resource_check_workers")
	if err != nil {
		return false, err
	}
	defer release()
	claimed := false
	err = s.registry.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkCoordination(tx, ctx); err != nil {
			return err
		}
		var running int64
		if err := tx.Model(&qProbeTaskModel{}).Where("direction = ? AND state = ? AND lease_until > ?", string(model.ProbeResourceCheck), "running", now).Count(&running).Error; err != nil {
			return err
		}
		if running >= 2 {
			return nil
		}
		result := tx.Model(&qProbeTaskModel{}).Where("id = ? AND state = ? AND created_at >= ?", row.ID, "pending", now.Add(-model.ResourceCheckQueueTimeout)).Updates(map[string]any{"state": "running", "updated_at": now, "lease_owner": s.owner, "lease_until": now.Add(ProbeLease)})
		claimed = result.RowsAffected == 1
		return result.Error
	})
	return claimed, err
}

func (s *ProbeTaskStore) CreateResourceCheck(ctx context.Context, task model.ProbeTask, capacity int) (uint64, error) {
	p := task.Experiment.ResourceCheck
	if task.Direction != model.ProbeResourceCheck || task.CaseID != 0 || task.Experiment.Version != model.ResourceCheckVersion || task.Experiment.UnsupportedReason() != "" || p == nil || capacity < 1 || len(p.Accounts) > 8 || len(p.Nodes) > model.ResourceCheckMaxGroups {
		return 0, errors.New("invalid resource check")
	}
	if p.ResourceID == 0 || p.Kind == "account" && task.DefendantAccountID != p.ResourceID || p.Kind == "node" && task.DefendantNodeID != p.ResourceID || p.Kind != "account" && p.Kind != "node" {
		return 0, errors.New("invalid resource target")
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
		query := active.Session(&gorm.Session{}).Where("direction = ?", string(model.ProbeResourceCheck))
		if p.Kind == "account" {
			query = query.Where("defendant_account_id = ?", p.ResourceID)
		} else {
			query = query.Where("defendant_node_id = ? AND defendant_account_id = 0", p.ResourceID)
		}
		if err := query.Order("id DESC").Limit(1).Find(&existing).Error; err != nil {
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

func (s *ProbeTaskStore) ListResourceChecks(ctx context.Context, kind string, ids []uint64) ([]model.ResourceCheck, error) {
	items := []model.ResourceCheck{}
	// A per-resource bound ensures a busy target cannot hide another selected
	// resource's active task behind a global recent-history limit.
	for _, id := range ids {
		var rows []qProbeTaskModel
		query := s.registry.db.WithContext(ctx).Where("direction = ?", string(model.ProbeResourceCheck))
		if kind == "account" {
			query = query.Where("defendant_account_id = ?", id)
		} else {
			query = query.Where("defendant_node_id = ? AND defendant_account_id = 0", id)
		}
		if err := query.Order("id DESC").Limit(5).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			check := model.ResourceCheck{ID: row.ID, Kind: kind, ResourceID: id, Model: probeExperimentFromJSON(row.ExperimentJSON).Baseline.Model, State: model.ProbeTaskState(row.State), CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt}
			var report model.ResourceCheckReport
			if json.Unmarshal([]byte(row.CheckReportJSON), &report) == nil && report.Version == model.ResourceCheckVersion {
				check.Report = &report
			}
			items = append(items, check)
		}
	}
	return items, nil
}

func (s *ProbeTaskStore) SaveResourceCheckProgress(ctx context.Context, id uint64, report model.ResourceCheckReport) error {
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	res := s.registry.db.WithContext(ctx).Model(&qProbeTaskModel{}).Where("id = ? AND direction = ? AND state = ? AND lease_owner = ? AND lease_until > ?", id, string(model.ProbeResourceCheck), "running", s.owner, time.Now().UTC()).Updates(map[string]any{"check_report_json": string(data), "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrProbeAlreadySettled
	}
	return nil
}
