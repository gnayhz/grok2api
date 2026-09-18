// Package investigator 是调查局(B4):取证探针任务队列。
//
// 证据引擎 = 积极模式(B1.3 决议3:立案即派)——生产流量只负责立案
// 触发,交叉定罪证据靠探针主动取证:
//   - account_differential 被告账号×健康出口差分(全降→有罪方向;
//     任一 clean→洗冤方向);差分必须验证出口 IP 真的变了(I8)。
//   - exit_jury 陪审员账号×被告出口(多数降→出口有罪;公理 B:
//     同时洗冤被告账号)。
//
// 执行器接口与任务队列分离:批3 提供队列/派发/结论入账,真实探针
// 执行器(SSO 取证/推理探测)在批5 管理面接线。
package investigator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// ErrInadmissible 结论不可采(I8:差分未验证 IP 变化)。
var ErrInadmissible = errors.New("investigator: 结论不可采")

// Executor 执行取证探针。真实实现(批5):SSO 差分重试/陪审员推理。
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

// Config 是调查局配置。
type Config struct {
	// DifferentialExits 账号差分使用的健康出口数上限。
	DifferentialExits int
	// JurorsPerExit 每被告出口的陪审员数。
	JurorsPerExit int
	// ProbeBudget 每案件每轮派发的任务预算。
	ProbeBudget int
}

// DefaultConfig 默认参数(面板可调留待批5)。JurorsPerExit=4 对齐出口
// 定罪见证门槛 ExitNeedN=4:陪审探针为主的部署若陪审员数低于门槛,
// 出口有罪永远差一票(批8 事故教训:证据引擎派发量必须够得着裁决门槛)。
func DefaultConfig() Config {
	return Config{DifferentialExits: 3, JurorsPerExit: 4, ProbeBudget: 7}
}

func (c Config) normalized() Config {
	if c.DifferentialExits <= 0 {
		c.DifferentialExits = 3
	}
	if c.JurorsPerExit <= 0 {
		c.JurorsPerExit = 4
	}
	if c.ProbeBudget <= 0 {
		c.ProbeBudget = 7
	}
	return c
}

// Service 是调查局。
type Service struct {
	mu       sync.RWMutex
	cfg      Config
	store    Store
	recorder Recorder
}

// SetConfig 热应用探针预算参数(差分出口数/陪审员数/每案预算)。
func (s *Service) SetConfig(cfg Config) {
	cfg = cfg.normalized()
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// Config 返回当前配置(管理面读回)。
func (s *Service) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Service) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// New 构建调查局。executor 为 nil 时不主动执行(RunDue no-op),
// 任务留队列待真实执行器(批5)认领。
func New(cfg Config, store Store, recorder Recorder) *Service {
	return &Service{cfg: cfg.normalized(), store: store, recorder: recorder}
}

// DispatchSpec 一次派发的内容。
// 与 court.DispatchSpec 保持字段同步,组合根 quality_judicial.go 逐字段复制。
type DispatchSpec struct {
	ControlAccounts []uint64
	ControlExits    []model.EpochKey
	CaseID          uint64
	Defendant       uint64
	// BaselineExit is the original degraded exit.  Every account
	// differential task compares this path against one HealthyExit target.
	// A zero value means that the case has no usable exit baseline (for
	// example, a direct connection), so the current court must not dispatch a
	// differential task for it.  The queue adapter still accepts a zero value
	// for legacy callers; those rows use the compatibility executor path.
	BaselineExit model.EpochKey
	// HealthyExits 差分可用的健康出口(未沾脏目标)。
	HealthyExits []model.EpochKey
	// CoRemandedExits 被告出口(陪审员探测对象),通常只有案件基线出口。
	CoRemandedExits []model.EpochKey
	// Jurors are the court-selected candidates from M06 eligibility and M07
	// identity facts; composition does not read account tables or select witnesses.
	Jurors []uint64
}

// DispatchForCase 立案即派(B1.3 决议3):差分+陪审员任务入队。
// 预算内截断;幂等与有界替代由调用方(court)的候选集和派发节流保证。
func (s *Service) DispatchForCase(ctx context.Context, spec DispatchSpec) (dispatched int, err error) {
	if s.store == nil {
		return 0, errors.New("investigator: task store 未接线")
	}
	cfg := s.config()
	controls := spec.ControlAccounts
	if len(controls) == 0 {
		controls = spec.Jurors
	}
	controlExits := spec.ControlExits
	if len(controlExits) == 0 {
		controlExits = spec.HealthyExits
	}
	budget := cfg.ProbeBudget
	differentialCount := 0
	seenComparisonNodes := map[uint64]struct{}{}
	if spec.BaselineExit.NodeID != 0 {
		seenComparisonNodes[spec.BaselineExit.NodeID] = struct{}{}
	}
	for _, exit := range spec.HealthyExits {
		if differentialCount >= cfg.DifferentialExits || budget <= 0 {
			break
		}
		// A direct event has no exit path to pin.  Also reject a target on
		// the baseline node here as a second guard against a malformed court
		// specification; node epochs are filtered by the court before this
		// adapter is called.
		if exit.NodeID == 0 {
			continue
		}
		if spec.BaselineExit.NodeID != 0 {
			if _, duplicate := seenComparisonNodes[exit.NodeID]; duplicate {
				continue
			}
			seenComparisonNodes[exit.NodeID] = struct{}{}
		}
		controlAccount := uint64(0)
		if len(controls) > 0 {
			controlAccount = controls[differentialCount%len(controls)]
		}
		if _, err := s.store.CreateProbeTask(ctx, model.ProbeTask{
			ControlAccountID: controlAccount, ControlNodeID: exit.NodeID, ControlEpoch: exit.Epoch,
			CaseID: spec.CaseID, Direction: model.ProbeAccountDifferential,
			DefendantAccountID: spec.Defendant, DefendantNodeID: exit.NodeID, DefendantEpoch: exit.Epoch,
			BaselineNodeID: spec.BaselineExit.NodeID, BaselineEpoch: spec.BaselineExit.Epoch,
		}); err != nil {
			return dispatched, err
		}
		dispatched++
		differentialCount++
		budget--
	}
	for _, exit := range spec.CoRemandedExits {
		for i, juror := range spec.Jurors {
			if i >= cfg.JurorsPerExit || budget <= 0 {
				break
			}
			controlExit := model.EpochKey{}
			if len(controlExits) > 0 {
				controlExit = controlExits[i%len(controlExits)]
			}
			if _, err := s.store.CreateProbeTask(ctx, model.ProbeTask{
				ControlAccountID: juror, ControlNodeID: controlExit.NodeID, ControlEpoch: controlExit.Epoch,
				CaseID: spec.CaseID, Direction: model.ProbeExitJury,
				DefendantAccountID: spec.Defendant, DefendantNodeID: exit.NodeID, DefendantEpoch: exit.Epoch,
				JurorAccountID: juror,
			}); err != nil {
				return dispatched, err
			}
			dispatched++
			budget--
		}
	}
	return dispatched, nil
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
			timeout := 2 * time.Minute
			if task.Direction == model.ProbeResourceCheck {
				timeout = model.ResourceCheckTimeout
			}
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
		if model.IsManualProbe(out.task.Direction) {
			// Manual reports have no court evidence projection or restriction.
			if err := s.settleProbe(out.task.ID, model.ProbeDone, out.result, now); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if admissible, reason := Admissible(out.task.Direction, out.result); !admissible {
			failed := out.result
			// 保留底层执行详情,追加不可采原因——只存统一术语会把
			// same-node/same-exit-ip/unresolved 等真实病根全部抹掉,
			// 面板只见"全失败"无从诊断(批9 事故)。
			if failed.Detail == "" {
				failed.Detail = reason
			} else {
				failed.Detail = failed.Detail + " | " + reason
			}
			if err := s.settleProbe(out.task.ID, model.ProbeFailed, failed, now); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Task rows are the durable source used by the experiment protocol.
		// A failed aggregate-window write must never erase a real measurement.
		writeCtx, cancel := context.WithTimeout(context.Background(), probeWriteTimeout)
		err := s.store.CompleteProbeTask(writeCtx, out.task.ID, model.ProbeDone, out.result, now)
		cancel()
		if errors.Is(err, model.ErrProbeAlreadySettled) {
			continue
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recordCtx, recordCancel := context.WithTimeout(context.Background(), probeWriteTimeout)
		if projections, ok := s.store.(projectionStore); ok {
			_, err = projections.ProcessProbeProjections(recordCtx, 1, s.recorder.Record)
		} else {
			err = s.recorder.Record(recordCtx, observationFromResult(out.task, out.result, now))
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
		recordCancel()

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

// Admissible 报告探针结论是否可采(I8 证据法):
// 差分方向的 degraded 结论必须验证出口 IP 真的变了,否则不定论;
// clean 结论本身是被告在该次路径上复现健康,可作为洗冤证据。
func Admissible(direction model.ProbeDirection, result model.ProbeTaskResult) (bool, string) {
	if result.Outcome == model.ProbeResultError {
		return false, "transport_error_not_evidence"
	}
	if result.Outcome != model.ProbeResultClean && result.Outcome != model.ProbeResultDegraded {
		return false, "unknown_probe_result"
	}
	if direction == model.ProbeAccountDifferential && result.Outcome == model.ProbeResultDegraded && !result.VerifiedIPChange {
		return false, "differential_without_ip_change_verification"
	}
	return true, ""
}

// observationFromResult shares the persisted projection schema with registry.
func observationFromResult(task model.ProbeTask, result model.ProbeTaskResult, at time.Time) model.Observation {
	return model.ProbeObservation(task, result, at)
}
