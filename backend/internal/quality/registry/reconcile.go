package registry

// 对账清扫(批9 事故族:清态不得寄生在"有在审案件"的评估循环里)。
// 案件全部销案后,仍然被押的出口/账号、在飞的探针必须有独立的回收
// 路径——否则它们永远停留原状,面板持续显示"取证在飞/节点羁押"。

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// CancelProbesForCase 结案即中止该案件的在飞取证(pending+running →
// cancelled):案件没有归宿的探针若不落地,面板永远显示"取证在飞"。
func (r *Registry) CancelProbesForCase(ctx context.Context, caseID uint64, reason string) (int64, error) {
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&qProbeTaskModel{}).
		Where("case_id = ? AND state IN ?", caseID,
			[]string{string(model.ProbePending), string(model.ProbeRunning)}).
		Updates(map[string]any{
			"state": string(model.ProbeCancelled), "detail": reason,
			"finished_at": now, "updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// CancelOrphanProbes 中止关联案件已不在审的在飞探针(兜底:任何结案
// 路径漏掉 CancelProbesForCase 都由这里收敛)。
func (r *Registry) CancelOrphanProbes(ctx context.Context, reason string) (int64, error) {
	if err := r.expirePendingResourceChecks(ctx); err != nil {
		return 0, err
	}
	openStatus := string(model.CaseInvestigating)
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&qProbeTaskModel{}).
		Where("NOT (direction = ? AND case_id = 0)", string(model.ProbeResourceCheck)).
		Where("state IN ? AND NOT EXISTS (SELECT 1 FROM q_case c WHERE c.id = q_probe_task.case_id AND c.status = ?)",
			[]string{string(model.ProbePending), string(model.ProbeRunning)}, openStatus).
		Updates(map[string]any{
			"state": string(model.ProbeCancelled), "detail": reason,
			"finished_at": now, "updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// ReclaimStaleRunningProbes 回收超租约的 running 探针:批上下文上限
// 远小于租约,超租约仍在 running 的只可能是进程重启/写回失败的僵尸
// (批9 事故:结论写回搭乘批超时 ctx,到期后写回全挂,行永久停留
// running)。落地为 cancelled 而非 failed——没有结论产出,不算失败。
func (r *Registry) ReclaimStaleRunningProbes(ctx context.Context, staleAfter time.Duration) (int64, error) {
	if staleAfter <= 0 {
		staleAfter = 5 * time.Minute
	}
	cutoff := time.Now().UTC().Add(-staleAfter)
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&qProbeTaskModel{}).
		Where("state = ? AND ((lease_until IS NOT NULL AND lease_until <= ?) OR (lease_until IS NULL AND updated_at < ?))", string(model.ProbeRunning), now, cutoff).
		Updates(map[string]any{
			"state": string(model.ProbeCancelled), "detail": "probe lease expired (worker lost)",
			"finished_at": now, "updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// ReclaimRunningProbes cancels only expired leases and legacy ownerless rows.
// A new process must never cancel a task owned by a live peer.
func (r *Registry) ReclaimRunningProbes(ctx context.Context, reason string) (int64, error) {
	if reason == "" {
		reason = "worker restarted"
	}
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&qProbeTaskModel{}).
		Where("state = ? AND (lease_owner = ? OR lease_until <= ?)", string(model.ProbeRunning), "", now).
		Updates(map[string]any{
			"state": string(model.ProbeCancelled), "detail": reason,
			"finished_at": now, "updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// ReconcileStaleExitParties marks old-epoch exit parties withdrawn. Advancing
// an IP epoch removes the live q_exit_state row immediately, but the case
// history still needs an explicit disposition so a closed pool conviction is
// not displayed forever as if its old IP were still held.
func (r *Registry) ReconcileStaleExitParties(ctx context.Context) (int, error) {
	if !r.inTransition {
		var n int
		err := r.withTransition(ctx, func(w *Registry) error { var err error; n, err = w.ReconcileStaleExitParties(ctx); return err })
		return n, err
	}

	r.transitionMu <- struct{}{}
	defer func() { <-r.transitionMu }()

	res := r.db.WithContext(ctx).Model(&qCasePartyModel{}).
		Where("kind = 'exit' AND disposition IN ? AND node_id <> 0", []string{"remanded", "sentenced"}).
		Where("epoch <> COALESCE((SELECT epoch FROM q_node_epoch n WHERE n.node_id = q_case_party.node_id), 0)").
		Updates(map[string]any{"disposition": "withdrawn", "updated_at": time.Now().UTC()})
	return int(res.RowsAffected), res.Error
}
