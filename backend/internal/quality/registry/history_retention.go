package registry

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// 历史保留清扫(批10):案件/探针历史表不同于观测表
// (证据局自带 7d 滚动清理)——此前无任何清理路径,
// 长期运行无界增长(降智波动期单日可产生百量级案件+
// 千量级探针行)。台账(q_degrade_ledger)为
// 永久保留的流行病学数据不清;状态表稀疏表示
// 天然有界;本清扫只针对历史明细表。

const (
	// caseHistoryRetention 结案案件(含当事方)的保留期。
	caseHistoryRetention = 30 * 24 * time.Hour
	// probeHistoryRetention 终态探针任务的保留期
	// (非终态永不清理——在飞任务属于活动状态)。
	probeHistoryRetention = 14 * 24 * time.Hour
)

// terminalProbeStates 探针终态(清理候选)。
var terminalProbeStates = []string{"done", "failed", "cancelled"}

// CleanExpiredCaseHistory 历史保留清扫:
//   - 终态探针任务超 probeHistoryRetention 删除;
//   - 结案案件超 caseHistoryRetention 删除(含当事方,
//     子行先于主行删除)。
//
// 在审案件与非终态探针永不清理(安全性:
// 清理不得影响活动案件与在飞取证)。
// 幂等:无事可清时零副作用。
func (r *Registry) CleanExpiredCaseHistory(ctx context.Context, now time.Time) (cases, parties, probes int64, err error) {
	defer func() {
		// 首次执行确认(健康扫掠清零时本来无日志——
		// 任务是否真在运行无法从日志验证;首次执行后
		// 永久安静,量可忽略)。
		if r.sweepOnce.CompareAndSwap(false, true) {
			if r.sweepLogger != nil {
				r.sweepLogger.Info("quality_history_retention_first_sweep", "cases", cases, "parties", parties, "probes", probes)
			}
		}
	}()
	probeCutoff := now.Add(-probeHistoryRetention)
	caseCutoff := now.Add(-caseHistoryRetention)
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A case and its measurements share one retention unit. Any active
		// restriction preserves its case, parties and tests for human review.
		expiredCases := tx.Model(&qCaseModel{}).Select("id").Where("closed_at IS NOT NULL AND closed_at < ?", caseCutoff).
			Where("verdict NOT IN ?", []string{"account_guilty", "exit_guilty"}).
			Where("NOT EXISTS (SELECT 1 FROM q_account_state a WHERE a.current_case_id=q_case.id)").
			Where("NOT EXISTS (SELECT 1 FROM q_exit_state e WHERE e.current_case_id=q_case.id)")
		res := tx.Where("case_id IN (?) OR (updated_at < ? AND state IN ? AND NOT EXISTS (SELECT 1 FROM q_case c WHERE c.id=q_probe_task.case_id))", expiredCases, probeCutoff, terminalProbeStates).Delete(&qProbeTaskModel{})
		if res.Error != nil {
			return res.Error
		}
		probes = res.RowsAffected
		if err := tx.Where("NOT EXISTS (SELECT 1 FROM q_probe_task p WHERE p.id=q_resource_check_target.task_id)").Delete(&qResourceCheckTargetModel{}).Error; err != nil {
			return err
		}

		partyRes := tx.Where("case_id IN (?)", expiredCases).
			Delete(&qCasePartyModel{})
		if partyRes.Error != nil {
			return partyRes.Error
		}
		parties = partyRes.RowsAffected

		caseRes := tx.Where("id IN (?)", expiredCases).
			Delete(&qCaseModel{})
		if caseRes.Error != nil {
			return caseRes.Error
		}
		cases = caseRes.RowsAffected
		return nil
	})
	return cases, parties, probes, err
}
