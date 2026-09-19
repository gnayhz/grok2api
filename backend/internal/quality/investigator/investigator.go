// Package investigator owns the bounded resource-proof task queue.
package investigator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// ErrInadmissible rejects unsupported experiments or unverified observations.
var ErrInadmissible = errors.New("investigator: 结论不可采")

// Executor executes the frozen full-response resource proof.
type Executor interface {
	Execute(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error)
}

// Recorder 是结论入账面(证据局实现)。
type Recorder interface {
	Record(ctx context.Context, obs model.Observation) error
}

// Store 是任务队列存取面(登记处实现)。任务/结论用 model 共享
// 词汇(model.ProbeTask/ProbeTaskResult),registry 与 investigator
// 无包间依赖。
type Store interface {
	CreateProbeTask(ctx context.Context, task model.ProbeTask) (uint64, error)
	ClaimPendingProbeTasks(ctx context.Context, limit int) ([]model.ProbeTask, error)
	CompleteProbeTask(ctx context.Context, taskID uint64, state model.ProbeTaskState, result model.ProbeTaskResult, finishedAt time.Time) error
}

// Service owns task execution and accepted observation recovery.
type Service struct {
	store    Store
	recorder Recorder
}

func New(store Store, recorder Recorder) *Service { return &Service{store: store, recorder: recorder} }

type DispatchSpec struct {
	CaseID       uint64
	Defendant    uint64
	BaselineExit model.EpochKey
}

func (s *Service) DispatchForCase(ctx context.Context, spec DispatchSpec) (int, error) {
	if s.store == nil {
		return 0, errors.New("investigator: task store unavailable")
	}
	_, err := s.store.CreateProbeTask(ctx, model.ProbeTask{CaseID: spec.CaseID, Direction: model.ProbeCaseProof, DefendantAccountID: spec.Defendant, DefendantNodeID: spec.BaselineExit.NodeID, DefendantEpoch: spec.BaselineExit.Epoch, BaselineNodeID: spec.BaselineExit.NodeID, BaselineEpoch: spec.BaselineExit.Epoch})
	if err != nil {
		return 0, err
	}
	return 1, nil
}

// runDue 认领并执行到期任务(执行器未接线时 no-op),返回本批执行数。
// 生产入口是 RunWorkers 的 worker 循环;导出包装 RunDue 已删除(零生产
// 调用),同包测试直接调用本方法。任务并发执行(limit 即并发度——探针
// 是真实上游调用,串行会让单批耗时=任务数×单任务时延,一个慢出口拖垮
// 整批;并发后整批耗时≈最慢单任务)。
// RunDueOnce 同步执行一批到期探针任务(集成测试与运维单步驱动使用;
// 常驻生产循环经 Run)。
func (s *Service) RunDueOnce(ctx context.Context, executor Executor, limit int) error {
	_, err := s.runDue(ctx, executor, limit)
	return err
}

func (s *Service) runDue(ctx context.Context, executor Executor, limit int) (int, error) {
	if executor == nil {
		return 0, nil
	}
	if s.store == nil {
		return 0, errors.New("investigator: task store 未接线")
	}
	if s.recorder == nil {
		return 0, errors.New("investigator: evidence recorder 未接线")
	}
	if limit <= 0 {
		limit = 8
	}
	tasks, err := s.store.ClaimPendingProbeTasks(ctx, limit)
	if err != nil {
		return 0, err
	}
	// 结论写回/证据入账不得搭乘批超时 ctx(批9 僵尸探针事故的根):
	// 写回上下文在执行完成后按次新建(见 writeContext)——若从批次
	// 开始就起算超时,慢批次执行完毕时写回窗口已过期,照样全挂
	// (上一版修复踩过的回归)。执行仍受批超时约束。
	type outcome struct {
		task        model.ProbeTask
		result      model.ProbeTaskResult
		execErr     error
		completedAt time.Time
	}
	results := make(chan outcome, len(tasks))
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		go func(task model.ProbeTask) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					results <- outcome{task: task, result: model.ProbeTaskResult{Outcome: model.ProbeResultError, FailureKind: "executor_failure", Detail: "executor_panic"}, execErr: errors.New("probe executor panic"), completedAt: time.Now().UTC()}
				}
			}()
			timeout := model.ResourceCheckTimeout
			taskCtx, taskCancel := context.WithTimeout(ctx, timeout)
			defer taskCancel()
			stopHeartbeat := s.heartbeat(taskCtx, task.ID, taskCancel)
			defer stopHeartbeat()
			result, execErr := executor.Execute(taskCtx, task)
			results <- outcome{task: task, result: result, execErr: execErr, completedAt: time.Now().UTC()}
		}(task)
	}
	// 结果通道在所有执行器退出后关闭,但不等整批执行完才开始写回。
	// 这样每个任务一返回就能独立落库,面板上的 finished_at 反映该任务
	// 的真实完成时间,而不是被最慢任务拖到批次末尾。通道有 len(tasks)
	// 个缓冲位,所以写回暂时变慢也不会把执行器卡死在发送结果上。
	go func() {
		wg.Wait()
		close(results)
	}()
	// 写回窗口从每个结果到达这里时起算,与其它任务的执行耗时无关。
	var firstErr error
	for out := range results {
		// The executor records completion before publishing its result. A slow
		// recorder or an earlier result must not move this task's finished_at to
		// the end of the batch.
		now := out.completedAt
		if now.IsZero() {
			now = time.Now().UTC()
		}
		if out.execErr != nil {
			state := model.ProbeFailed
			if errors.Is(out.execErr, context.Canceled) || errors.Is(out.execErr, context.DeadlineExceeded) {
				state = model.ProbeCancelled
			}
			if err := s.settleProbe(out.task.ID, state, out.result, now); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.settleProbe(out.task.ID, model.ProbeDone, out.result, now); err != nil && firstErr == nil {
			firstErr = err
		}

	}
	return len(tasks), firstErr
}

// settleProbe 落探针结论。执行期间任务被结案中止/租约回收时,
// 后到的结论是良性竞态丢弃——不得当作批错误
// 上抡(cancelled 不可复活,批10 竞态修复)。真实执行已产生的
// 观测仍按原时序入证据局(证据与案件解耦,真实信号不丢)。
func (s *Service) settleProbe(taskID uint64, state model.ProbeTaskState, result model.ProbeTaskResult, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeWriteTimeout)
	defer cancel()
	if err := s.store.CompleteProbeTask(ctx, taskID, state, result, now); err != nil {
		if errors.Is(err, model.ErrProbeAlreadySettled) {
			return nil
		}
		return err
	}
	return nil
}

// probeWriteTimeout 单次结论写回/证据入账的超时(按次新建,不受执行
// 耗时挤占)。包级变量仅为测试可注入。
var probeWriteTimeout = 15 * time.Second
