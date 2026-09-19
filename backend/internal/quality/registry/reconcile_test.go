package registry

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func newReconcileRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := Open(context.Background(), Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "reconcile.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func insertCaseRow(t *testing.T, reg *Registry, id uint64, status string) {
	t.Helper()
	now := time.Now().UTC()
	if err := reg.DB().Exec(
		"INSERT INTO q_case (id, status, verdict, opened_at, updated_at) VALUES (?, ?, '', ?, ?)",
		id, status, now, now,
	).Error; err != nil {
		t.Fatal(err)
	}
}

func insertProbeRow(t *testing.T, reg *Registry, caseID uint64, state string, updatedAt time.Time) {
	t.Helper()
	if err := reg.DB().Exec(
		"INSERT INTO q_probe_task (case_id, direction, state, created_at, updated_at) VALUES (?, 'exit_jury', ?, ?, ?)",
		caseID, state, updatedAt, updatedAt,
	).Error; err != nil {
		t.Fatal(err)
	}
}

func probeStateOf(t *testing.T, reg *Registry, caseID uint64) []string {
	t.Helper()
	var states []string
	if err := reg.DB().Raw("SELECT state FROM q_probe_task WHERE case_id = ? ORDER BY id", caseID).Scan(&states).Error; err != nil {
		t.Fatal(err)
	}
	return states
}

// TestProbeReclaimAndOrphanCancel 锚定探针清态:超租约 running 僵尸落地、
// 孤儿在飞任务中止、结案即中止;新鲜在飞不受租约回收误伤。
func TestProbeReclaimAndOrphanCancel(t *testing.T) {
	reg := newReconcileRegistry(t)
	ctx := context.Background()
	insertCaseRow(t, reg, 1, "investigating")
	insertCaseRow(t, reg, 2, "dismissed")

	now := time.Now().UTC()
	insertProbeRow(t, reg, 1, "running", now.Add(-10*time.Minute)) // 僵尸(超租约)
	insertProbeRow(t, reg, 1, "running", now)                      // 新鲜在飞
	insertProbeRow(t, reg, 2, "pending", now)                      // 孤儿(案件已销)
	insertProbeRow(t, reg, 2, "running", now)                      // 孤儿 running

	n, err := reg.ReclaimStaleRunningProbes(ctx, 5*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("超租约应回收 1 条, got %d err=%v", n, err)
	}
	states := probeStateOf(t, reg, 1)
	if states[0] != string(model.ProbeCancelled) {
		t.Fatalf("僵尸应落地 cancelled, got %s", states[0])
	}
	if states[1] != string(model.ProbeRunning) {
		t.Fatalf("新鲜在飞不得被租约回收, got %s", states[1])
	}

	n, err = reg.CancelOrphanProbes(ctx, "案件已结,取证中止")
	if err != nil || n != 2 {
		t.Fatalf("孤儿在飞应中止 2 条, got %d err=%v", n, err)
	}
	for i, state := range probeStateOf(t, reg, 2) {
		if state != string(model.ProbeCancelled) {
			t.Fatalf("孤儿探针 %d 应 cancelled, got %s", i, state)
		}
	}

	n, err = reg.CancelProbesForCase(ctx, 1, "案件已结,取证中止")
	if err != nil || n != 1 {
		t.Fatalf("结案应中止该案在飞 1 条, got %d err=%v", n, err)
	}
	if state := probeStateOf(t, reg, 1)[1]; state != string(model.ProbeCancelled) {
		t.Fatalf("结案后新鲜在飞也应中止, got %s", state)
	}
}

// TestProbeStateCheckAcceptsCancelled 锚定迁移:cancelled 状态必须能落库
// (历史 CHECK 不含该值时 Open 迁移负责扩约束)。
func TestProbeStateCheckAcceptsCancelled(t *testing.T) {
	reg := newReconcileRegistry(t)
	insertCaseRow(t, reg, 9, "investigating")
	insertProbeRow(t, reg, 9, "pending", time.Now().UTC())
	if _, err := reg.CancelProbesForCase(context.Background(), 9, "测试中止"); err != nil {
		t.Fatalf("cancelled 必须通过 CHECK 约束: %v", err)
	}
}

// TestCompleteProbeTaskDoesNotResurrectCancelled 锚定批10 竞态修复:
// 结案中止/租约回收后,执行工迟到的结论写回必须被拒绝——cancelled
// 是终态,不得被 done/failed 复活(否则中止语义倒转,面板与统计失真)。
func TestCompleteProbeTaskDoesNotResurrectCancelled(t *testing.T) {
	ctx := context.Background()
	reg, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "probe.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	store := NewProbeTaskStore(reg)
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{CaseID: 9, Direction: model.ProbeExitJury, JurorAccountID: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPendingProbeTasks(ctx, 4); err != nil {
		t.Fatal(err)
	}
	cancelled, err := reg.CancelProbesForCase(ctx, 9, "案件已结")
	if err != nil || cancelled != 1 {
		t.Fatalf("结案中止 = %d, %v", cancelled, err)
	}
	late := store.CompleteProbeTask(ctx, id, model.ProbeDone, model.ProbeTaskResult{Outcome: model.ProbeResultDegraded, Detail: "late"}, time.Now().UTC())
	if !errors.Is(late, model.ErrProbeAlreadySettled) {
		t.Fatalf("迟到结论必须被拒(ErrProbeAlreadySettled), got %v", late)
	}
	tasks, err := store.ListProbeTasksForCase(ctx, 9)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == id && (task.State != model.ProbeCancelled || task.Result != "") {
			t.Fatalf("cancelled 行不得被复活: state=%s result=%s", task.State, task.Result)
		}
	}
	// 未认领的 pending 任务同样不得直接落结论(必须先经 running)。
	id2, err := store.CreateProbeTask(ctx, model.ProbeTask{CaseID: 10, Direction: model.ProbeExitJury, JurorAccountID: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteProbeTask(ctx, id2, model.ProbeDone, model.ProbeTaskResult{Outcome: model.ProbeResultClean}, time.Now().UTC()); !errors.Is(err, model.ErrProbeAlreadySettled) {
		t.Fatalf("pending 任务不得跳过认领直接完成, got %v", err)
	}
}

// TestConcurrentProbeClaimsAreExclusive 锚定调查局队列并发语义:
// 多个 worker 同时扫 pending 时,条件更新 0 行的 worker 不得继续执行
// 同一任务,每个任务最多返回给一个认领者。
func TestConcurrentProbeClaimsAreExclusive(t *testing.T) {
	reg := newReconcileRegistry(t)
	store := NewProbeTaskStore(reg)
	ctx := context.Background()
	const taskCount = 40
	for i := 0; i < taskCount; i++ {
		if _, err := store.CreateProbeTask(ctx, model.ProbeTask{
			CaseID: 1, Direction: model.ProbeExitJury, DefendantNodeID: 3, JurorAccountID: uint64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	claimedIDs := make(chan uint64, taskCount*2)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempts := 0; attempts < taskCount; attempts++ {
				tasks, err := store.ClaimPendingProbeTasks(ctx, 1)
				if err != nil {
					t.Errorf("并发认领: %v", err)
					return
				}
				if len(tasks) == 0 {
					continue
				}
				claimedIDs <- tasks[0].ID
			}
		}()
	}
	workers.Wait()
	close(claimedIDs)

	seen := make(map[uint64]struct{}, taskCount)
	for id := range claimedIDs {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("任务 %d 被多个 worker 认领", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != taskCount {
		t.Fatalf("全部任务都应恰好认领一次, got %d/%d", len(seen), taskCount)
	}
}

// TestCleanExpiredCaseHistory 锚定历史保留清扫(批10):
// 终态探针/结案案件超期删除;在审案件、
// 非终态探针、近期结案案件不受影响;幂等。
func TestCleanExpiredCaseHistory(t *testing.T) {
	ctx := context.Background()
	reg, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "retention.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)

	// 老结案+当事方+老终态探针 → 应被清理。
	oldClosed, err := reg.CreateCase(ctx, old, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertParty(ctx, model.PartyRecord{CaseID: oldClosed, Kind: model.PartyAccount, AccountID: 1, Role: model.RoleDefendant, Disposition: model.DispositionRemanded}); err != nil {
		t.Fatal(err)
	}
	probeStore := NewProbeTaskStore(reg)
	oldProbe, err := probeStore.CreateProbeTask(ctx, model.ProbeTask{CaseID: oldClosed, Direction: model.ProbeExitJury})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probeStore.ClaimPendingProbeTasks(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if err := probeStore.CompleteProbeTask(ctx, oldProbe, model.ProbeDone, model.ProbeTaskResult{Outcome: model.ProbeResultClean}, old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := reg.CloseCase(ctx, oldClosed, model.CaseDismissed, model.VerdictDismissed, old.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 老结案的 updated_at 也回拔(否则 CloseCase 刚刚刷新它)。
	if err := reg.db.WithContext(ctx).Model(&qCaseModel{}).Where("id = ?", oldClosed).
		Updates(map[string]any{"updated_at": old, "closed_at": old.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := reg.db.WithContext(ctx).Model(&qProbeTaskModel{}).Where("id = ?", oldProbe).
		Update("updated_at", old).Error; err != nil {
		t.Fatal(err)
	}

	// 在审案件(同样很老)→ 不得清理。
	oldOpen, err := reg.CreateCase(ctx, old, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.db.WithContext(ctx).Model(&qCaseModel{}).Where("id = ?", oldOpen).Update("updated_at", old).Error; err != nil {
		t.Fatal(err)
	}
	// 老但非终态(pending)探针 → 不得清理。
	stalePending, err := probeStore.CreateProbeTask(ctx, model.ProbeTask{CaseID: oldOpen, Direction: model.ProbeExitJury})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.db.WithContext(ctx).Model(&qProbeTaskModel{}).Where("id = ?", stalePending).Update("updated_at", old).Error; err != nil {
		t.Fatal(err)
	}
	// 近期结案 → 不得清理。
	recentClosed, err := reg.CreateCase(ctx, now.Add(-time.Hour), "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CloseCase(ctx, recentClosed, model.CaseDismissed, model.VerdictDismissed, now); err != nil {
		t.Fatal(err)
	}

	cases, parties, probes, err := reg.CleanExpiredCaseHistory(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if cases != 1 || parties != 1 || probes != 1 {
		t.Fatalf("应清 1 案 1 方 1 探针, got cases=%d parties=%d probes=%d", cases, parties, probes)
	}
	// 在审/非终态/近期结案保留。
	var openCount int64
	reg.db.WithContext(ctx).Model(&qCaseModel{}).Where("id = ?", oldOpen).Count(&openCount)
	if openCount != 1 {
		t.Fatal("在审案件不得被清理")
	}
	var pendingCount int64
	reg.db.WithContext(ctx).Model(&qProbeTaskModel{}).Where("id = ?", stalePending).Count(&pendingCount)
	if pendingCount != 1 {
		t.Fatal("非终态探针不得被清理")
	}
	var recentCount int64
	reg.db.WithContext(ctx).Model(&qCaseModel{}).Where("id = ?", recentClosed).Count(&recentCount)
	if recentCount != 1 {
		t.Fatal("近期结案不得被清理")
	}
	// 幂等:二次清理零增量。
	cases2, parties2, probes2, err := reg.CleanExpiredCaseHistory(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if cases2 != 0 || parties2 != 0 || probes2 != 0 {
		t.Fatalf("幂等违约: cases=%d parties=%d probes=%d", cases2, parties2, probes2)
	}
}

// TestCleanExpiredCaseHistoryFirstSweepLogsOnce 锚定首执行确认:
// 健康扫掠(清零)也必须留下一次执行证据,
// 且只留一次(后续永久安静)。
func TestCleanExpiredCaseHistoryFirstSweepLogsOnce(t *testing.T) {
	reg, err := Open(context.Background(), Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "ret-once.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if reg.sweepOnce.Load() {
		t.Fatal("新建实例不得预置首执行标记")
	}
	if _, _, _, err := reg.CleanExpiredCaseHistory(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if !reg.sweepOnce.Load() {
		t.Fatal("首次执行后标记必须置位")
	}
}
