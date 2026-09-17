package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// TestResetQualityStateAnchorsD7 锚定 D7 切换清零:
// 六张状态表清空+缓存复位;三张保留表(epoch 档案/台账/身份组)不动。
func TestResetQualityStateAnchorsD7(t *testing.T) {
	ctx := context.Background()
	registry, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatalf("打开登记处: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	now := time.Now().UTC()
	// 造状态:账号羁押+出口羁押+案件+观测+台账+IP 档案。
	if err := registry.TransitionAccount(ctx, model.AccountTransitionRequest{
		AccountID: 11, To: model.AccountRemanded, CaseID: 1,
	}); err != nil {
		t.Fatalf("羁押账号: %v", err)
	}
	if err := registry.TransitionExit(ctx, model.ExitTransitionRequest{
		NodeID: 7, Epoch: 0, To: model.ExitRemanded, CaseID: 1,
	}); err != nil {
		t.Fatalf("羁押出口: %v", err)
	}
	caseID, err := registry.CreateCase(ctx, now, "{}")
	if err != nil {
		t.Fatalf("立案: %v", err)
	}
	if err := registry.DB().Exec(
		"INSERT INTO q_observation (at, account_id, node_id, epoch, source, outcome, rule) VALUES (?,?,?,?,?,?,?)",
		now, 11, 7, 0, "traffic", "degraded", "item_done").Error; err != nil {
		t.Fatalf("造观测: %v", err)
	}
	if err := registry.AppendDegrade(ctx, 7, 0, "203.0.113.7", now); err != nil {
		t.Fatalf("台账: %v", err)
	}
	if err := registry.RecordExitIdentity(ctx, 7, model.ExitIdentityFromAggregate("203.0.113.7")); err != nil {
		t.Fatalf("IP 档案: %v", err)
	}

	if err := registry.ResetQualityState(ctx); err != nil {
		t.Fatalf("清零: %v", err)
	}

	if got := registry.AccountsTracked(); got != 0 {
		t.Fatalf("账号状态行未清: %d", got)
	}
	if got := registry.ExitsTracked(); got != 0 {
		t.Fatalf("出口状态行未清: %d", got)
	}
	// 资格全部复位(无行=可调度)。
	if !registry.AccountEligible(11) || !registry.ExitEligible(7) {
		t.Fatal("清零后资格必须复位")
	}
	var cases int64
	registry.DB().Table("q_case").Count(&cases)
	if cases != 0 {
		t.Fatalf("案件未清: %d", cases)
	}
	if caseID == 0 {
		t.Fatal("前置失败:未立上案")
	}
	var obs int64
	registry.DB().Table("q_observation").Count(&obs)
	if obs != 0 {
		t.Fatalf("观测未清: %d", obs)
	}
	// 保留表:台账与 IP 档案仍在。
	var ledger int64
	registry.DB().Table("q_degrade_ledger").Count(&ledger)
	if ledger != 1 {
		t.Fatalf("台账必须保留: %d", ledger)
	}
	var epochRows int64
	registry.DB().Table("q_ip_epoch").Count(&epochRows)
	if epochRows != 1 {
		t.Fatalf("IP 档案必须保留: %d", epochRows)
	}
}

// TestResetQualityStateDBRollsBackOnFailure 锚定切换清零原子性:
// 清零过程中任一状态表操作失败时,此前已删除的表也必须回滚,不能留下
// 半清零数据库。
func TestResetQualityStateDBRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	registry, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if _, err := registry.CreateCase(ctx, time.Now().UTC(), "{}"); err != nil {
		t.Fatal(err)
	}
	if err := registry.DB().Exec("DROP TABLE q_probe_task").Error; err != nil {
		t.Fatal(err)
	}
	if err := ResetQualityStateDB(ctx, registry.DB()); err == nil {
		t.Fatal("缺少状态表时清零必须失败")
	}
	var cases int64
	if err := registry.DB().Table("q_case").Count(&cases).Error; err != nil {
		t.Fatal(err)
	}
	if cases != 1 {
		t.Fatalf("清零失败必须整体回滚, q_case=%d", cases)
	}
}
