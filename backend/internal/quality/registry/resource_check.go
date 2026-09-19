package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
)

// The generation owner has two shared measurement slots. A batch must wait in
// the durable queue instead of having eight workers race for those two slots.
func (s *ProbeTaskStore) claimResourceCheck(ctx context.Context, row qProbeTaskModel, now time.Time) (bool, error) {
	if probeExperimentFromJSON(row.ExperimentJSON).Version != model.ResourceCheckVersion {
		err := s.registry.db.WithContext(ctx).Model(&qProbeTaskModel{}).Where("id = ? AND state = ?", row.ID, "pending").Updates(map[string]any{"state": "cancelled", "detail": "unsupported_experiment_version", "finished_at": now, "updated_at": now}).Error
		return false, err
	}
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
		if err := tx.Model(&qProbeTaskModel{}).Where("direction IN ? AND state = ? AND lease_until > ?", []string{string(model.ProbeResourceCheck), string(model.ProbeCaseProof)}, "running", now).Count(&running).Error; err != nil {
			return err
		}
		if running >= 2 {
			return nil
		}
		if row.Direction == string(model.ProbeCaseProof) {
			plan := probeExperimentFromJSON(row.ExperimentJSON).ResourceCheck
			var count int64
			if err := tx.Model(&qCaseModel{}).Where("id = ? AND status = ?", row.CaseID, "investigating").Count(&count).Error; err != nil {
				return err
			}
			if count != 1 || plan == nil || !now.Before(plan.DeadlineAt) {
				return tx.Model(&qProbeTaskModel{}).Where("id = ? AND state = ?", row.ID, "pending").Updates(map[string]any{"state": "cancelled", "detail": "case_deadline_or_closed", "finished_at": now, "updated_at": now}).Error
			}
		}
		result := tx.Model(&qProbeTaskModel{}).Where("id = ? AND state = ? AND created_at >= ?", row.ID, "pending", now.Add(-model.ResourceCheckQueueTimeout)).Updates(map[string]any{"state": "running", "updated_at": now, "lease_owner": s.owner, "lease_until": now.Add(ProbeLease)})
		claimed = result.RowsAffected == 1
		return result.Error
	})
	return claimed, err
}

// One queue row owns a whole batch. A separate index links each target without
// duplicating state, leases or measurements across per-resource workers.
func (s *ProbeTaskStore) CreateResourceCheckBatch(ctx context.Context, tasks []model.ProbeTask, capacity int) ([]model.ResourceSubmission, error) {
	if len(tasks) == 0 || len(tasks) > 32 || capacity < 1 {
		return nil, errors.New("invalid resource batch")
	}
	for _, t := range tasks {
		p := t.Experiment.ResourceCheck
		if t.Direction != model.ProbeResourceCheck || t.CaseID != 0 || t.Experiment.Version != model.ResourceCheckVersion || t.Experiment.UnsupportedReason() != "" || p == nil || p.ResourceID == 0 || (p.Kind != "account" && p.Kind != "node") || len(p.Accounts) > model.ResourceCheckMaxAccounts || len(p.Nodes) > model.ResourceCheckMaxNodes {
			return nil, errors.New("invalid resource check")
		}
		a, b := t.Experiment.Baseline, tasks[0].Experiment.Baseline
		if a.Provider != b.Provider || a.Model != b.Model || a.Revision != b.Revision || a.RuleVersion != b.RuleVersion {
			return nil, errors.New("batch specification mismatch")
		}
	}
	ctx, release, err := s.registry.Coordinate(ctx, "resource_checks")
	if err != nil {
		return nil, err
	}
	defer release()
	items := make([]model.ResourceSubmission, len(tasks))
	err = s.registry.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkCoordination(tx, ctx); err != nil {
			return err
		}
		active := tx.Model(&qProbeTaskModel{}).Where("direction = ? AND state IN ?", string(model.ProbeResourceCheck), []string{"pending", "running"})
		var used int64
		if err := active.Select("COALESCE(SUM(manual_slots),0)").Scan(&used).Error; err != nil {
			return err
		}
		accepted := []int{}
		plan := *tasks[0].Experiment.ResourceCheck
		plan.Targets = nil
		seen := map[string]bool{}
		for i, t := range tasks {
			p := t.Experiment.ResourceCheck
			items[i].ResourceID = p.ResourceID
			key := fmt.Sprintf("%s:%d", p.Kind, p.ResourceID)
			if seen[key] {
				items[i].Error = "duplicate_target"
				continue
			}
			seen[key] = true
			var existing []qProbeTaskModel
			q := tx.Where("direction = ? AND state IN ?", string(model.ProbeResourceCheck), []string{"pending", "running"}).Where("id IN (SELECT task_id FROM q_resource_check_target WHERE kind = ? AND resource_id = ?)", p.Kind, p.ResourceID)
			if err := q.Order("id DESC").Limit(1).Find(&existing).Error; err != nil {
				return err
			}
			if len(existing) > 0 {
				old := probeExperimentFromJSON(existing[0].ExperimentJSON)
				a, b := old.Baseline, t.Experiment.Baseline
				if old.Version != t.Experiment.Version || a.Model != b.Model || a.Provider != b.Provider || a.Revision != b.Revision || a.RuleVersion != b.RuleVersion {
					items[i].Error = "active_spec_conflict"
				} else {
					items[i].ID = existing[0].ID
				}
				continue
			}
			if used >= int64(capacity) {
				items[i].Error = "queue_full"
				continue
			}
			used++
			accepted = append(accepted, i)
			plan.Targets = append(plan.Targets, model.ResourceTarget{Kind: p.Kind, ResourceID: p.ResourceID, UnavailableReason: p.UnavailableReason})
		}
		if len(accepted) == 0 {
			return nil
		}
		plan.MaxCalls = len(accepted) + 4
		plan.Seed = uint64(time.Now().UnixNano()) & ((1 << 63) - 1)
		first := tasks[accepted[0]]
		plan.Kind, plan.ResourceID = first.Experiment.ResourceCheck.Kind, first.Experiment.ResourceCheck.ResourceID
		first.Experiment.ResourceCheck = &plan
		first.Experiment.Baseline.AccountID = 0
		store := &ProbeTaskStore{registry: &Registry{db: tx, inTransition: true}, owner: s.owner}
		id, err := store.CreateProbeTask(ctx, first)
		if err != nil {
			return err
		}
		if err := tx.Model(&qProbeTaskModel{}).Where("id = ?", id).Update("manual_slots", len(accepted)).Error; err != nil {
			return err
		}
		for _, i := range accepted {
			p := tasks[i].Experiment.ResourceCheck
			if err := tx.Create(&qResourceCheckTargetModel{TaskID: id, Kind: p.Kind, ResourceID: p.ResourceID}).Error; err != nil {
				return err
			}
			items[i].ID = id
		}
		return nil
	})
	return items, err
}

func (s *ProbeTaskStore) ListResourceChecks(ctx context.Context, kind string, ids []uint64) ([]model.ResourceCheck, error) {
	items := []model.ResourceCheck{}
	for _, id := range ids {
		var rows []qProbeTaskModel
		q := s.registry.db.WithContext(ctx).Where("direction = ?", string(model.ProbeResourceCheck)).Where("id IN (SELECT task_id FROM q_resource_check_target WHERE kind = ? AND resource_id = ?)", kind, id)
		if err := q.Order("id DESC").Limit(5).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			check := model.ResourceCheck{ID: row.ID, Kind: kind, ResourceID: id, Model: probeExperimentFromJSON(row.ExperimentJSON).Baseline.Model, State: model.ProbeTaskState(row.State), CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt}
			var report model.ResourceCheckReport
			if json.Unmarshal([]byte(row.CheckReportJSON), &report) == nil && report.Version == model.ResourceCheckVersion {
				report = model.ResourceReportFor(report, kind, id)
				check.Report = &report
			}
			items = append(items, check)
		}
	}
	return items, nil
}

// Persisting the reservation before measurement bounds calls across failures.
// Revision CAS rejects stale same-owner writes as well as foreign owners.
func (s *ProbeTaskStore) SaveResourceCheckProgress(ctx context.Context, id uint64, report model.ResourceCheckReport) error {
	if report.Version != model.ResourceCheckVersion || report.Revision == 0 || report.Calls < 0 || report.Calls > report.MaxCalls {
		return model.ErrProbeAlreadySettled
	}
	var row qProbeTaskModel
	if err := s.registry.db.WithContext(ctx).Where("id = ? AND check_revision = ?", id, report.Revision-1).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ErrProbeAlreadySettled
		}
		return err
	}
	plan := probeExperimentFromJSON(row.ExperimentJSON).ResourceCheck
	var previous model.ResourceCheckReport
	if row.CheckRevision != 0 && json.Unmarshal([]byte(row.CheckReportJSON), &previous) != nil {
		return model.ErrProbeAlreadySettled
	}
	// The frozen plan, not the writer's report, owns the physical budget.
	// The final CAS fences this read against concurrent progress/completion.
	if plan == nil || report.MaxCalls != plan.MaxCalls || report.Calls < previous.Calls || report.Calls > previous.Calls+1 {
		return model.ErrProbeAlreadySettled
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	res := s.registry.db.WithContext(ctx).Model(&qProbeTaskModel{}).Where("id = ? AND direction IN ? AND state = ? AND lease_owner = ? AND lease_until > ? AND check_revision = ?", id, []string{string(model.ProbeResourceCheck), string(model.ProbeCaseProof)}, "running", s.owner, time.Now().UTC(), report.Revision-1).Updates(map[string]any{"check_report_json": string(data), "check_revision": report.Revision, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return model.ErrProbeAlreadySettled
	}
	return nil
}

func (r *Registry) expirePendingResourceChecks(ctx context.Context) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Model(&qProbeTaskModel{}).Where("state = ? AND direction = ? AND created_at < ?", "pending", string(model.ProbeResourceCheck), now.Add(-model.ResourceCheckQueueTimeout)).Updates(map[string]any{"state": "cancelled", "detail": "resource check queue deadline", "finished_at": now, "updated_at": now}).Error
}
