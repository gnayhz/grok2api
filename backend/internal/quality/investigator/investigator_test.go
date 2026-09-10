package investigator

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type memStore struct {
	mu          sync.Mutex
	nextID      uint64
	tasks       []model.ProbeTask
	byID        map[uint64]int
	states      map[uint64]model.ProbeTaskState
	completions []completionRecord
}

type completionRecord struct {
	taskID     uint64
	state      model.ProbeTaskState
	finishedAt time.Time
}

func newMemStore() *memStore {
	return &memStore{nextID: 1, byID: map[uint64]int{}, states: map[uint64]model.ProbeTaskState{}}
}

func (s *memStore) CreateProbeTask(_ context.Context, task model.ProbeTask) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task.ID = s.nextID
	s.nextID++
	s.byID[task.ID] = len(s.tasks)
	s.tasks = append(s.tasks, task)
	return task.ID, nil
}

func (s *memStore) ClaimPendingProbeTasks(_ context.Context, limit int) ([]model.ProbeTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var claimed []model.ProbeTask
	for i := range s.tasks {
		if len(claimed) >= limit {
			break
		}
		if state := s.states[s.tasks[i].ID]; state != "" && state != model.ProbePending {
			continue
		}
		s.states[s.tasks[i].ID] = model.ProbeRunning
		claimed = append(claimed, s.tasks[i])
	}
	return claimed, nil
}

func (s *memStore) CompleteProbeTask(ctx context.Context, taskID uint64, state model.ProbeTaskState, result model.ProbeTaskResult, finishedAt time.Time) error {
	// 模拟真实 DB 写:搭乘已取消 ctx 的写回必然失败——这正是批9
	// 僵尸探针的根(写回失败被吞,行永久停留 running)。
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 与真实存储同语义:仅 running 行可落结论,其余视为已结算。
	if s.states[taskID] != model.ProbeRunning {
		return model.ErrProbeAlreadySettled
	}
	s.states[taskID] = state
	s.completions = append(s.completions, completionRecord{taskID: taskID, state: state, finishedAt: finishedAt})
	return nil
}

// cancelTask 模拟执行期间结案中止(竞态注入)。
func (s *memStore) cancelTask(taskID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[taskID] = model.ProbeCancelled
}

type memRecorder struct {
	mu   sync.Mutex
	obs  []model.Observation
	fail error
}

func (r *memRecorder) Record(_ context.Context, obs model.Observation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.obs = append(r.obs, obs)
	return r.fail
}

// TestDispatchForCaseBudget 锚定立案即派+预算:差分×健康出口上限+
// 陪审员×被告出口,预算内截断。
func TestDispatchForCaseBudget(t *testing.T) {
	store := newMemStore()
	service := New(Config{DifferentialExits: 2, JurorsPerExit: 3, ProbeBudget: 5}, store, &memRecorder{})
	spec := DispatchSpec{
		CaseID:    9,
		Defendant: 42,
		HealthyExits: []model.EpochKey{
			{NodeID: 1}, {NodeID: 2}, {NodeID: 3},
		},
		CoRemandedExits: []model.EpochKey{{NodeID: 7}, {NodeID: 8}},
		Jurors:          []uint64{100, 101, 102, 103},
	}
	dispatched, err := service.DispatchForCase(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// 差分 2(上限)+陪审员 min(3×2, 余预算 3)=5。
	if dispatched != 5 {
		t.Fatalf("预算内派发 = %d, want 5", dispatched)
	}
	var differential, jury int
	for _, task := range store.tasks {
		switch task.Direction {
		case model.ProbeAccountDifferential:
			differential++
		case model.ProbeExitJury:
			jury++
		}
	}
	if differential != 2 || jury != 3 {
		t.Fatalf("差分 %d 陪审 %d", differential, jury)
	}
}

// TestDispatchForCaseCarriesDifferentialPaths 锚定差分任务的路径语义:
// baseline 是原始降智出口,defendant node/epoch 是对比出口;同一对比节点
// 的不同 epoch 不能在同一轮重复占槽。
func TestDispatchForCaseCarriesDifferentialPaths(t *testing.T) {
	store := newMemStore()
	service := New(DefaultConfig(), store, &memRecorder{})
	dispatched, err := service.DispatchForCase(context.Background(), DispatchSpec{
		CaseID: 10, Defendant: 42,
		BaselineExit: model.EpochKey{NodeID: 116, Epoch: 0},
		HealthyExits: []model.EpochKey{
			{NodeID: 107, Epoch: 138},
			{NodeID: 107, Epoch: 139},
			{NodeID: 111, Epoch: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dispatched != 2 {
		t.Fatalf("同节点不同 epoch 只能保留一个对比目标, dispatched=%d", dispatched)
	}
	if len(store.tasks) != 2 {
		t.Fatalf("tasks=%+v", store.tasks)
	}
	for _, task := range store.tasks {
		if task.Direction != model.ProbeAccountDifferential {
			t.Fatalf("unexpected task=%+v", task)
		}
		if task.BaselineNodeID != 116 || task.BaselineEpoch != 0 {
			t.Fatalf("baseline path lost: %+v", task)
		}
		if task.DefendantNodeID == 116 {
			t.Fatalf("comparison target must not equal baseline: %+v", task)
		}
	}
}

func TestDispatchForCaseUsesOnlyCoreProbeGroups(t *testing.T) {
	store := newMemStore()
	service := New(DefaultConfig(), store, &memRecorder{})
	dispatched, err := service.DispatchForCase(context.Background(), DispatchSpec{
		CaseID: 11, Defendant: 42,
		BaselineExit:    model.EpochKey{NodeID: 9, Epoch: 2},
		HealthyExits:    []model.EpochKey{{NodeID: 1}, {NodeID: 2}, {NodeID: 3}, {NodeID: 4}},
		CoRemandedExits: []model.EpochKey{{NodeID: 9, Epoch: 2}},
		Jurors:          []uint64{100, 101, 102, 103},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dispatched != 7 || len(store.tasks) != 7 {
		t.Fatalf("simple round must dispatch 3 differential + 4 jury tasks, dispatched=%d tasks=%d", dispatched, len(store.tasks))
	}
	var differential, jury int
	for _, task := range store.tasks {
		switch task.Direction {
		case model.ProbeAccountDifferential:
			differential++
		case model.ProbeExitJury:
			jury++
		default:
			t.Fatalf("unexpected probe direction in simple round: %+v", task)
		}
	}
	if differential != 3 || jury != 4 {
		t.Fatalf("simple probe groups = differential %d jury %d", differential, jury)
	}
}

// TestDispatchForCaseRequiresStore 锚定调查局组装契约:未接任务存储时
// 必须返回可诊断错误,不能在立案路径上 nil pointer panic。
func TestDispatchForCaseRequiresStore(t *testing.T) {
	service := New(DefaultConfig(), nil, &memRecorder{})
	if _, err := service.DispatchForCase(context.Background(), DispatchSpec{CaseID: 1, Defendant: 7}); err == nil {
		t.Fatal("缺少任务存储时必须返回错误")
	}
}

// TestAdmissibleI8 锚定 I8:差分 degraded 结论必须验证出口 IP 真的变了;
// clean 结论可直接作为洗冤证据,传输错误不可采。
func TestAdmissibleI8(t *testing.T) {
	if ok, reason := Admissible(model.ProbeAccountDifferential, model.ProbeTaskResult{
		Outcome: model.ProbeResultClean, VerifiedIPChange: false,
	}); !ok || reason != "" {
		t.Fatalf("差分 clean 结论应可采, ok=%v reason=%q", ok, reason)
	}
	if ok, reason := Admissible(model.ProbeAccountDifferential, model.ProbeTaskResult{
		Outcome: model.ProbeResultDegraded, VerifiedIPChange: false,
	}); ok || reason != "differential_without_ip_change_verification" {
		t.Fatalf("未验证 IP 变化的差分 degraded 结论必须不可采, ok=%v reason=%q", ok, reason)
	}
	// 陪审员方向不涉 IP 变化验证(同一出口是探测对象本身)。
	if ok, _ := Admissible(model.ProbeExitJury, model.ProbeTaskResult{
		Outcome: model.ProbeResultDegraded,
	}); !ok {
		t.Fatal("陪审员结论可采")
	}
	if ok, _ := Admissible(model.ProbeExitJury, model.ProbeTaskResult{
		Outcome: model.ProbeResultError,
	}); ok {
		t.Fatal("传输错误不可采(I10)")
	}
	if ok, reason := Admissible(model.ProbeExitJury, model.ProbeTaskResult{}); ok || reason != "unknown_probe_result" {
		t.Fatalf("未知探针结果不得升级为 clean 证据, ok=%v reason=%q", ok, reason)
	}
}

// TestRunDueRecordsProbeObservations 执行器结论入账证据局
// (source=probe),不可采结论标失败不入账。
func TestRunDueRecordsProbeObservations(t *testing.T) {
	store := newMemStore()
	service := New(DefaultConfig(), store, &memRecorder{})
	// 两个任务:可采的陪审员降智 + 不可采的差分。
	id1, _ := store.CreateProbeTask(context.Background(), model.ProbeTask{
		CaseID: 1, Direction: model.ProbeExitJury, DefendantAccountID: 7,
		DefendantNodeID: 5, JurorAccountID: 88,
	})
	id2, _ := store.CreateProbeTask(context.Background(), model.ProbeTask{
		CaseID: 1, Direction: model.ProbeAccountDifferential, DefendantAccountID: 7,
		DefendantNodeID: 5,
	})
	recorder := &memRecorder{}
	service = New(DefaultConfig(), store, recorder)
	executor := stubExecutor{results: map[uint64]model.ProbeTaskResult{
		id1: {Outcome: model.ProbeResultDegraded, Detail: "jury_rule"},
		id2: {Outcome: model.ProbeResultDegraded, VerifiedIPChange: false},
	}}
	if err := service.RunDue(context.Background(), executor, 8); err != nil {
		t.Fatal(err)
	}
	if len(recorder.obs) != 1 {
		t.Fatalf("只有可采结论入账 = %d", len(recorder.obs))
	}
	obs := recorder.obs[0]
	if obs.Source != model.SourceProbe || obs.AccountID != 88 || obs.Exit.NodeID != 5 || obs.Outcome != model.OutcomeDegraded {
		t.Fatalf("陪审员观测主体应是陪审员: %+v", obs)
	}
}

type staggeredExecutor struct{}

func (staggeredExecutor) Execute(_ context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	if task.ID == 1 {
		time.Sleep(15 * time.Millisecond)
	} else {
		time.Sleep(140 * time.Millisecond)
	}
	return model.ProbeTaskResult{Outcome: model.ProbeResultClean, Detail: "staggered"}, nil
}

// TestRunDuePersistsEachTaskAtItsOwnCompletion 锚定调查局及时落库:
// 快任务完成后必须先写回,不能等同批慢任务结束后再统一写 finished_at。
func TestRunDuePersistsEachTaskAtItsOwnCompletion(t *testing.T) {
	store := newMemStore()
	recorder := &memRecorder{}
	service := New(DefaultConfig(), store, recorder)
	for i := 0; i < 2; i++ {
		if _, err := store.CreateProbeTask(context.Background(), model.ProbeTask{
			CaseID: 1, Direction: model.ProbeExitJury, DefendantNodeID: 5, JurorAccountID: uint64(80 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- service.RunDue(context.Background(), staggeredExecutor{}, 2) }()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		count := len(store.completions)
		store.mu.Unlock()
		if count >= 1 {
			if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
				t.Fatalf("first completion was delayed until the slow task: %s", elapsed)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	store.mu.Lock()
	count := len(store.completions)
	store.mu.Unlock()
	if count < 1 {
		t.Fatal("fast task was not persisted before the observation deadline")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.completions) != 2 || store.completions[0].finishedAt.IsZero() || store.completions[1].finishedAt.IsZero() {
		t.Fatalf("each task must carry its own completion timestamp: %+v", store.completions)
	}
	if store.completions[0].finishedAt.Equal(store.completions[1].finishedAt) {
		t.Fatal("completion timestamps must not be stamped once at batch end")
	}
}

// TestRunDueRetainsMeasurementWhenAggregateWriteFails 锚定证据一致性:
// 观测入账失败时探针不能伪装成 done,否则法院会把不存在的 clean 证据
// 用于洗冤/翻案。
func TestRunDueRetainsMeasurementWhenAggregateWriteFails(t *testing.T) {
	store := newMemStore()
	recorder := &memRecorder{fail: errors.New("evidence store unavailable")}
	service := New(DefaultConfig(), store, recorder)
	id, err := store.CreateProbeTask(context.Background(), model.ProbeTask{
		CaseID: 1, Direction: model.ProbeExitJury, DefendantAccountID: 7,
		DefendantNodeID: 5, JurorAccountID: 88,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunDue(context.Background(), stubExecutor{results: map[uint64]model.ProbeTaskResult{
		id: {Outcome: model.ProbeResultClean, Detail: "jury_clean"},
	}}, 1); err == nil {
		t.Fatal("证据写入失败必须向调用方可见")
	}
	if state := store.states[id]; state != model.ProbeDone {
		t.Fatalf("聚合写入失败不能抹去已持久化的测试结果, got %s", state)
	}
	if len(store.completions) != 1 || store.completions[0].state != model.ProbeDone {
		t.Fatalf("测试结果仍应落 done, got %+v", store.completions)
	}
}

type stubExecutor struct {
	results map[uint64]model.ProbeTaskResult
}

func (e stubExecutor) Execute(_ context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	if result, ok := e.results[task.ID]; ok {
		return result, nil
	}
	return model.ProbeTaskResult{Outcome: model.ProbeResultError}, nil
}

// TestProbeTaskStoreAgainstRegistry 队列存储真实落库往返。
func TestProbeTaskStoreAgainstRegistry(t *testing.T) {
	ctx := context.Background()
	qualityRegistry, err := qualityregistry.Open(ctx, qualityregistry.Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "inv.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer qualityRegistry.Close()
	store := qualityregistry.NewProbeTaskStore(qualityRegistry)
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{
		CaseID: 3, Direction: model.ProbeExitJury, DefendantAccountID: 1, DefendantNodeID: 2, JurorAccountID: 9,
	})
	if err != nil || id == 0 {
		t.Fatalf("创建任务 = %d, %v", id, err)
	}
	tasks, err := store.ClaimPendingProbeTasks(ctx, 4)
	if err != nil || len(tasks) != 1 || tasks[0].JurorAccountID != 9 {
		t.Fatalf("认领 = %+v, %v", tasks, err)
	}
	if err := store.CompleteProbeTask(ctx, id, model.ProbeDone, model.ProbeTaskResult{
		Outcome: model.ProbeResultDegraded, Detail: "jury",
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 完成后不再可认领。
	tasks, err = store.ClaimPendingProbeTasks(ctx, 4)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("已完成任务不得再认领 = %+v, %v", tasks, err)
	}
}

// deadlineExecutor 等批 ctx 取消才返回(模拟最慢探针撞上批超时)。
type deadlineExecutor struct{}

func (deadlineExecutor) Execute(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	<-ctx.Done()
	return model.ProbeTaskResult{Outcome: model.ProbeResultError, Detail: "batch deadline"}, ctx.Err()
}

// TestRunDueSurvivesBatchDeadline 锚定批9 僵尸探针根因修复:批 ctx 到期
// 后,结论写回必须用独立上下文照常落地——不得遗留 running。
func TestRunDueSurvivesBatchDeadline(t *testing.T) {
	store := newMemStore()
	svc := New(DefaultConfig(), store, &memRecorder{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunDue(ctx, deadlineExecutor{}, 4); err != nil {
		t.Fatalf("写回不得因批超时失败: %v", err)
	}
	if len(store.completions) != 1 {
		t.Fatalf("结论必须落地 1 条, got %d", len(store.completions))
	}
	if c := store.completions[0]; c.taskID != id || c.state != model.ProbeCancelled {
		t.Fatalf("工作批次中断应落地 cancelled, got %+v", c)
	}
}

// slowExecutor 执行耗时超过写回窗口(模拟慢差分批次)。
type slowExecutor struct{ d time.Duration }

func (e slowExecutor) Execute(context.Context, model.ProbeTask) (model.ProbeTaskResult, error) {
	time.Sleep(e.d)
	return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded, Detail: "slow batch"}, nil
}

// TestRunDueWriteWindowIndependentOfExecution 锚定写回窗口回归:写回
// 超时必须在执行完成后起算——若从批次开始起算,慢批次执行完毕时窗口
// 已过期,结论照样全挂(修复途中踩过的二次事故:执行全成功、写回全
// 超时,探针依旧僵尸)。
func TestRunDueWriteWindowIndependentOfExecution(t *testing.T) {
	orig := probeWriteTimeout
	probeWriteTimeout = 30 * time.Millisecond
	defer func() { probeWriteTimeout = orig }()
	store := newMemStore()
	svc := New(DefaultConfig(), store, &memRecorder{})
	ctx := context.Background()
	if _, err := store.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 1}); err != nil {
		t.Fatal(err)
	}
	// 执行 80ms > 写回窗口 30ms:窗口若从批次开始起算必过期。
	if err := svc.RunDue(ctx, slowExecutor{d: 80 * time.Millisecond}, 4); err != nil {
		t.Fatalf("慢批执行不得挤垮写回窗口: %v", err)
	}
	if len(store.completions) != 1 || store.completions[0].state != model.ProbeDone {
		t.Fatalf("慢批结论必须照常落地, got %+v", store.completions)
	}
}

// midFlightCancelExecutor 执行期间触发结案中止(模拟竞态:
// 探针在飞时案件被销,随后结论才到)。
type midFlightCancelExecutor struct{ store *memStore }

func (e midFlightCancelExecutor) Execute(_ context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	e.store.cancelTask(task.ID)
	return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded, Detail: "arrived after dismissal"}, nil
}

// TestRunDueLateConclusionDoesNotResurrectCancelled 锚定批10 竞态
// 修复:执行期间任务被中止后,后到的结论必须良性丢弃
// ——RunDue 不报错,行保持 cancelled。
func TestRunDueLateConclusionDoesNotResurrectCancelled(t *testing.T) {
	store := newMemStore()
	svc := New(DefaultConfig(), store, &memRecorder{})
	ctx := context.Background()
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunDue(ctx, midFlightCancelExecutor{store: store}, 4); err != nil {
		t.Fatalf("已中止任务的迟到结论不得当作批错误: %v", err)
	}
	if state := store.states[id]; state != model.ProbeCancelled {
		t.Fatalf("行必须保持 cancelled, got %s", state)
	}
	for _, c := range store.completions {
		if c.taskID == id {
			t.Fatal("已中止任务不得产生完成记录(复活)")
		}
	}
}

func TestProbeObservationRetainsActualAttemptInsteadOfTaskPlan(t *testing.T) {
	actual := attemptmeta.Identity{ID: "probe/1", AccountID: 13, Revision: 7, RuleVersion: "r1", Path: attemptmeta.Path{NodeID: 31, Epoch: 4, Status: attemptmeta.PathRegistered}}
	obs := observationFromResult(model.ProbeTask{DefendantAccountID: 99, DefendantNodeID: 98, DefendantEpoch: 97}, model.ProbeTaskResult{Outcome: model.ProbeResultError, Attempt: actual}, time.Now())
	if obs.AccountID != 13 || obs.Exit.NodeID != 31 || obs.Exit.Epoch != 4 || obs.Attempt != actual {
		t.Fatalf("rewrote physical identity: %+v", obs)
	}
}
