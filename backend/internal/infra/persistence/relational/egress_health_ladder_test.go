package relational

import (
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// SQL 侧注入的冷却阶梯阈值必须与 domain.CooldownDuration 完全一致：
// 两条实现（HealthState.Apply 与 ApplyEgressHealthObservation 的 SQL CASE）
// 只允许有一个数值源，本测试锁定注入参数不发生漂移。
func TestCooldownLadderCaseMatchesDomainCooldownDuration(t *testing.T) {
	observedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := cooldownLadderCase(observedAt, 1)
	sql := expr.SQL
	if !strings.Contains(sql, "CASE WHEN last_error = ? THEN cooldown_until") || !strings.HasSuffix(sql, "ELSE ? END") {
		t.Fatalf("unexpected ladder CASE shape: %s", sql)
	}
	if rungs := cooldownLadderRungs(); rungs != 5 {
		t.Fatalf("cooldownLadderRungs() = %d, want 5 (30s<<min(fc-1,4) saturates at fc=5)", rungs)
	}
	// gorm.Expr 的 Vars 顺序与占位符一一对应：last_error，随后每个梯级
	// (failure_count+? 阈值的 count, 该梯级的冷却时刻)，最后是 ELSE 兜底。
	vars := expr.Vars
	if len(vars) != 1+4*2+1 {
		t.Fatalf("ladder CASE vars = %d, want 10", len(vars))
	}
	for failureCount := 1; failureCount <= 6; failureCount++ {
		want := observedAt.Add(egress.CooldownDuration(failureCount))
		// 梯级 fc=2..rungs 的 vars 索引：1 + (rungs-fc)*2 + 1；fc=1 走 ELSE 兜底；
		// fc>rungs 与饱和梯级同值（CooldownDuration 封顶）。
		effective := min(failureCount, cooldownLadderRungs())
		var got time.Time
		if effective == 1 {
			got = vars[len(vars)-1].(time.Time)
		} else {
			got = vars[1+(cooldownLadderRungs()-effective)*2+1].(time.Time)
		}
		if !got.Equal(want) {
			t.Fatalf("fc=%d: injected threshold %v, want %v (domain.CooldownDuration=%v)", failureCount, got, want, egress.CooldownDuration(failureCount))
		}
	}
}
