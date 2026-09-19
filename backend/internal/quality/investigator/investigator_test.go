package investigator

import (
	"context"

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

// TestDispatchForCaseRequiresStore 锚定调查局组装契约:未接任务存储时
// 必须返回可诊断错误,不能在立案路径上 nil pointer panic。
func TestDispatchForCaseRequiresStore(t *testing.T) {
	service := New(nil, &memRecorder{})
	if _, err := service.DispatchForCase(context.Background(), DispatchSpec{CaseID: 1, Defendant: 7}); err == nil {
		t.Fatal("缺少任务存储时必须返回错误")
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
	service := New(store, recorder)
	for i := 0; i < 2; i++ {
		if _, err := store.CreateProbeTask(context.Background(), model.ProbeTask{
			CaseID: 1, Direction: model.ProbeExitJury, DefendantNodeID: 5, JurorAccountID: uint64(80 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, runErr := service.runDue(context.Background(), staggeredExecutor{}, 2)
		done <- runErr
	}()
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
	svc := New(store, &memRecorder{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.runDue(ctx, deadlineExecutor{}, 4); err != nil {
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
	svc := New(store, &memRecorder{})
	ctx := context.Background()
	if _, err := store.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 1}); err != nil {
		t.Fatal(err)
	}
	// 执行 80ms > 写回窗口 30ms:窗口若从批次开始起算必过期。
	if _, err := svc.runDue(ctx, slowExecutor{d: 80 * time.Millisecond}, 4); err != nil {
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
	svc := New(store, &memRecorder{})
	ctx := context.Background()
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.runDue(ctx, midFlightCancelExecutor{store: store}, 4); err != nil {
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
