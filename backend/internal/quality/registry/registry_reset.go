package registry

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// stateTables is the explicit maintenance reset scope. Startup and background
// reconciliation do not call this operation; archives, the degrade ledger and
// identity groups are outside its scope.
//
// q_incident_closure is deliberately unbounded: quality/README 记录该投影
// "保留到显式质量状态重置"，用于阻止迟到事件在原始案件被保留策略清理后
// 重新立案。它只随结案事务增长（每 (account_id, node_id, epoch) 一行），
// 因此不进入 retention sweep——清理它会静默削弱去重保证。
var stateTables = []string{
	"q_probe_projection",
	"q_account_state",
	"q_exit_state",
	"q_case",
	"q_case_party",
	"q_incident_closure",
	"q_observation",
	"q_probe_task",
}

// ResetQualityState 是面向运维的显式维护入口：清零上表并重建本注册表缓存。
// 它是有意不接线到组合根、也不对外暴露 HTTP 的破坏性操作（清空全部质量
// 限制与案件），因此保持为进程内库入口，由运维工具在隔离环境调用；不是
// 升级步骤，任何后台循环都不得调用它。
func (r *Registry) ResetQualityState(ctx context.Context) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.ResetQualityState(ctx) })
	}
	if err := ResetQualityStateDB(ctx, r.db); err != nil {
		return err
	}
	return r.rebuildCache(ctx)
}

// ResetQualityStateDB 对给定数据库执行同一显式维护清零；缓存刷新由调用方
// 负责。它是 ResetQualityState 的数据库级形态，供运维工具与注册表测试使用，
// 生产组合根不调用。
func ResetQualityStateDB(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, table := range stateTables {
			if err := tx.Exec("DELETE FROM " + table).Error; err != nil {
				return fmt.Errorf("清零 %s: %w", table, err)
			}
		}
		return nil
	})
}
