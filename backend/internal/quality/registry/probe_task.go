package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 调查局任务队列存储(B3 q_probe_task)。court 派发、investigator
// 认领/入账;每个案件只有一轮核心差分/陪审任务,失败路径可由 court
// 在同一有限生命周期内追加替代任务。

// ProbeTaskStore 实现 investigator.Store(任务/结论用 model 共享词汇)。
type ProbeTaskStore struct {
	registry *Registry
	owner    string
}

// NewProbeTaskStore 返回任务队列存取面。
func NewProbeTaskStore(registry *Registry) *ProbeTaskStore {
	return &ProbeTaskStore{registry: registry, owner: uuid.NewString()}
}

// CreateProbeTask 入队一个取证任务。
func (s *ProbeTaskStore) CreateProbeTask(ctx context.Context, task model.ProbeTask) (uint64, error) {
	if !s.registry.inTransition {
		var id uint64
		err := s.registry.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := checkCoordination(tx, ctx); err != nil {
				return err
			}
			// Queue insertion needs only its frozen case, not a state cache.
			// The shared case lock serializes with settlement/cancellation.
			store := &ProbeTaskStore{registry: &Registry{db: tx, inTransition: true}, owner: s.owner}
			var err error
			id, err = store.CreateProbeTask(ctx, task)
			return err
		})
		return id, err
	}

	now := time.Now().UTC()
	// The persisted case specification is authoritative for initial dispatch
	// and every replacement. Dispatch callers cannot silently change it.
	if task.CaseID != 0 {
		var cases []qCaseModel
		if err := s.registry.db.WithContext(ctx).Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ?", task.CaseID).Limit(1).Find(&cases).Error; err != nil {
			return 0, err
		}
		if len(cases) > 0 {
			if cases[0].Status != string(model.CaseInvestigating) {
				return 0, model.ErrProbeAlreadySettled
			}
			var envelope struct {
				Policy struct {
					Experiment model.ProbeExperiment `json:"experiment"`
				} `json:"policy"`
			}
			if err := json.Unmarshal([]byte(cases[0].EvidenceJSON), &envelope); err == nil && envelope.Policy.Experiment.Version != "" {
				task.Experiment = envelope.Policy.Experiment
			}
		}
	}
	experimentJSON, err := json.Marshal(task.Experiment)
	if err != nil {
		return 0, err
	}
	row := qProbeTaskModel{
		ExperimentJSON: string(experimentJSON),
		CaseID:         task.CaseID, Direction: string(task.Direction),
		DefendantAccountID: task.DefendantAccountID,
		DefendantNodeID:    task.DefendantNodeID, DefendantEpoch: task.DefendantEpoch,
		BaselineNodeID: task.BaselineNodeID, BaselineEpoch: task.BaselineEpoch,
		JurorAccountID:   task.JurorAccountID,
		ControlAccountID: task.ControlAccountID, ControlNodeID: task.ControlNodeID, ControlEpoch: task.ControlEpoch,
		State: string(model.ProbePending), CreatedAt: now, UpdatedAt: now,
	}
	if err := s.registry.db.WithContext(ctx).Create(&row).Error; err != nil {
		return 0, err
	}
	return row.ID, nil
}

// ClaimPendingProbeTasks 认领待执行任务并置 running。
func (s *ProbeTaskStore) ClaimPendingProbeTasks(ctx context.Context, limit int) ([]model.ProbeTask, error) {
	if limit <= 0 {
		limit = 8
	}
	var rows []qProbeTaskModel
	if err := s.registry.db.WithContext(ctx).
		Where("state = ?", string(model.ProbePending)).
		Order("id").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	tasks := make([]model.ProbeTask, 0, len(rows))
	now := time.Now().UTC()
	for _, row := range rows {
		claimed := s.registry.db.WithContext(ctx).Model(&qProbeTaskModel{}).
			Where("id = ? AND state = ?", row.ID, string(model.ProbePending)).
			Updates(map[string]any{"state": string(model.ProbeRunning), "updated_at": now, "lease_owner": s.owner, "lease_until": now.Add(ProbeLease)})
		if claimed.Error != nil {
			return tasks, claimed.Error
		}
		if claimed.RowsAffected == 0 {
			// 另一个 worker 可能已经抢走这行;条件更新 0 行
			// 不是执行失败,但绝不能把未真正认领的任务返回给调用方。
			continue
		}
		tasks = append(tasks, probeTaskFromRow(row))
	}
	return tasks, nil
}

// CompleteProbeTask 落结论。仅 running 行可落:执行期被结案中止
// (CancelProbesForCase)或租约回收(ReclaimStaleRunningProbes)的任务,
// 后到的结论必须丢弃——cancelled 不可复活(批10 竞态修复:
// 无条件按 id 覆写会把中止语义倒转,面板与统计失真)。
func (s *ProbeTaskStore) CompleteProbeTask(ctx context.Context, taskID uint64, state model.ProbeTaskState, result model.ProbeTaskResult, finishedAt time.Time) error {
	attemptJSON, err := json.Marshal(result.Attempt)
	if err != nil {
		return err
	}
	controlJSON, err := json.Marshal(result.ControlAttempt)
	if err != nil {
		return err
	}
	updates := map[string]any{
		"attempt_json": string(attemptJSON), "control_attempt_json": string(controlJSON), "projection_version": 1,
		"state": string(state), "updated_at": time.Now().UTC(), "finished_at": finishedAt,
		"failure_kind": result.FailureKind, "path_key": result.PathKey,
		"control_outcome": string(result.ControlOutcome), "control_detail": result.ControlDetail,
		"control_path_key": result.ControlPathKey, "control_verified": result.ControlVerified,
		"result": string(result.Outcome), "verified_ip_change": result.VerifiedIPChange, "detail": result.Detail,
	}
	return s.registry.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&qProbeTaskModel{}).
			Where("id = ? AND state = ? AND lease_owner = ? AND lease_until > ?", taskID, string(model.ProbeRunning), s.owner, time.Now().UTC()).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: task %d", model.ErrProbeAlreadySettled, taskID)
		}
		if state != model.ProbeDone {
			return nil
		}
		var row qProbeTaskModel
		if err := tx.First(&row, "id = ?", taskID).Error; err != nil {
			return err
		}
		obs := model.ProbeObservation(probeTaskFromRow(row), result, finishedAt)
		payload, err := json.Marshal(obs)
		if err != nil {
			return err
		}
		return tx.Create(&qProbeProjectionModel{TaskID: taskID, Payload: string(payload), ReadyAt: time.Now().UTC()}).Error
	})
}

// ListProbeTasks 列出最近探针任务(面板/调查局观测)。
func (s *ProbeTaskStore) ListProbeTasks(ctx context.Context, limit int) ([]model.ProbeTaskView, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []qProbeTaskModel
	if err := s.registry.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]model.ProbeTaskView, 0, len(rows))
	for _, row := range rows {
		views = append(views, model.ProbeTaskView{
			Experiment: probeExperimentFromJSON(row.ExperimentJSON),
			Attempt:    probeAttemptIdentity(row.AttemptJSON), ControlAttempt: probeAttemptIdentity(row.ControlAttemptJSON),
			ID: row.ID, CaseID: row.CaseID, Direction: model.ProbeDirection(row.Direction),
			ControlAccountID: row.ControlAccountID, ControlNodeID: row.ControlNodeID, ControlEpoch: row.ControlEpoch,
			FailureKind: row.FailureKind, PathKey: row.PathKey, ControlOutcome: model.ProbeResult(row.ControlOutcome),
			ControlDetail: row.ControlDetail, ControlPathKey: row.ControlPathKey, ControlVerified: row.ControlVerified,
			Defendant: row.DefendantAccountID, NodeID: row.DefendantNodeID, Epoch: row.DefendantEpoch,
			BaselineNodeID: row.BaselineNodeID, BaselineEpoch: row.BaselineEpoch,
			Juror: row.JurorAccountID, State: model.ProbeTaskState(row.State),
			Result: model.ProbeResult(row.Result), VerifiedIPChange: row.VerifiedIPChange, Detail: row.Detail,
			CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt,
		})
	}
	return views, nil
}

// ListProbeTasksForCase returns every task belonging to one case. The direct
// tribunal workflow evaluates one finite investigation round, so it must read
// the complete round rather than a recent global page.
func (s *ProbeTaskStore) ListProbeTasksForCase(ctx context.Context, caseID uint64) ([]model.ProbeTaskView, error) {
	var rows []qProbeTaskModel
	if err := s.registry.db.WithContext(ctx).
		Where("case_id = ?", caseID).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]model.ProbeTaskView, 0, len(rows))
	for _, row := range rows {
		views = append(views, model.ProbeTaskView{
			Experiment: probeExperimentFromJSON(row.ExperimentJSON),
			Attempt:    probeAttemptIdentity(row.AttemptJSON), ControlAttempt: probeAttemptIdentity(row.ControlAttemptJSON),
			ID: row.ID, CaseID: row.CaseID, Direction: model.ProbeDirection(row.Direction),
			ControlAccountID: row.ControlAccountID, ControlNodeID: row.ControlNodeID, ControlEpoch: row.ControlEpoch,
			FailureKind: row.FailureKind, PathKey: row.PathKey, ControlOutcome: model.ProbeResult(row.ControlOutcome),
			ControlDetail: row.ControlDetail, ControlPathKey: row.ControlPathKey, ControlVerified: row.ControlVerified,
			Defendant: row.DefendantAccountID, NodeID: row.DefendantNodeID, Epoch: row.DefendantEpoch,
			BaselineNodeID: row.BaselineNodeID, BaselineEpoch: row.BaselineEpoch,
			Juror: row.JurorAccountID, State: model.ProbeTaskState(row.State),
			Result: model.ProbeResult(row.Result), VerifiedIPChange: row.VerifiedIPChange, Detail: row.Detail,
			CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt,
		})
	}
	return views, nil
}

// Legacy rows have no physical identity; never reconstruct one from task plans.
func probeAttemptIdentity(raw string) attemptmeta.Identity {
	var identity attemptmeta.Identity
	if json.Unmarshal([]byte(raw), &identity) != nil {
		return attemptmeta.Identity{}
	}
	return identity
}

func probeTaskFromRow(row qProbeTaskModel) model.ProbeTask {
	return model.ProbeTask{Experiment: probeExperimentFromJSON(row.ExperimentJSON), ID: row.ID, CaseID: row.CaseID, Direction: model.ProbeDirection(row.Direction),
		DefendantAccountID: row.DefendantAccountID, DefendantNodeID: row.DefendantNodeID, DefendantEpoch: row.DefendantEpoch,
		BaselineNodeID: row.BaselineNodeID, BaselineEpoch: row.BaselineEpoch, JurorAccountID: row.JurorAccountID,
		ControlAccountID: row.ControlAccountID, ControlNodeID: row.ControlNodeID, ControlEpoch: row.ControlEpoch}
}

const ProbeLease = 3 * time.Minute

func (s *ProbeTaskStore) RenewProbeTask(ctx context.Context, id uint64) error {
	now := time.Now().UTC()
	res := s.registry.db.WithContext(ctx).Model(&qProbeTaskModel{}).
		Where("id = ? AND state = ? AND lease_owner = ? AND lease_until > ?", id, string(model.ProbeRunning), s.owner, now).
		Updates(map[string]any{"lease_until": now.Add(ProbeLease), "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrProbeAlreadySettled
	}
	return nil
}

func probeExperimentFromJSON(raw string) model.ProbeExperiment {
	var spec model.ProbeExperiment
	_ = json.Unmarshal([]byte(raw), &spec)
	return spec
}
