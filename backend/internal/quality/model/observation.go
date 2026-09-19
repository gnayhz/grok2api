package model

import (
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"time"
)

// Outcome 是一次质量观测的判定(I10:传输 error 不能作为直接降智票,
// 同时剔除出质量分子与分母——不可采质量证据;案件层可在独立控制条件下
// 识别重复传输异常模式,但不能把单次 error 当作 degraded)。
type Outcome string

const (
	// OutcomeDelivered is the legacy positive observation value. Production admissions
	// are journal-only; new probe positives require admission and protocol completion,
	// without asserting content correctness.
	OutcomeDelivered Outcome = "delivered"
	// OutcomeDegraded records a classified admission rejection. Attribution requires separate controlled comparisons.
	OutcomeDegraded Outcome = "degraded"
	// OutcomeError 传输失败:不可采。
	OutcomeError Outcome = "error"
)

// Decidable 报告该判定是否可进入统计(降智/放行)。
func (o Outcome) Decidable() bool {
	return o == OutcomeDelivered || o == OutcomeDegraded
}

// Source 区分观测来源:有机流量判定(守卫)与调查局注入探测(B3 观测双来源)。
type Source string

const (
	// SourceTraffic 生产流量旁路判定。
	SourceTraffic Source = "traffic"
	// SourceProbe 调查局探针结论。
	SourceProbe Source = "probe"
)

// Observation 是证据局的最小证据单元:一次 (账号,出口) 质量观测。
// 出口按 (节点,IP-epoch) 键控(I15);Rule 是守卫规则指纹或探测结论
// 摘要——证据链不含 IP 明文、账号名、密钥(I24)。
type Observation struct {
	EventID   string
	Attempt   attemptmeta.Identity
	At        time.Time
	AccountID uint64
	Exit      EpochKey
	Source    Source
	Outcome   Outcome
	Rule      string
}

// ProbeDirection 是调查局取证探针的方向(B1.3 证据引擎)。
type ProbeDirection string

const (
	// ProbeAccountDifferential 被告账号×多个健康出口(差分):
	// 全降→账号有罪方向;任一 clean→账号洗冤方向。
	ProbeAccountDifferential ProbeDirection = "account_differential"
	// ProbeExitJury 陪审员账号×被告出口:多数降→出口有罪
	// (公理 B:同时洗冤被告账号)。
	ProbeExitJury ProbeDirection = "exit_jury"
)

// ProbeTaskState 是探针任务的生命周期。
type ProbeTaskState string

const (
	// ProbePending 已立案待执行(立案即派)。
	ProbePending ProbeTaskState = "pending"
	// ProbeRunning 执行中。
	ProbeRunning ProbeTaskState = "running"
	// ProbeDone 已完成,结果可用。
	ProbeDone ProbeTaskState = "done"
	// ProbeFailed 执行失败(可重试或过期)。
	ProbeFailed ProbeTaskState = "failed"
	// ProbeCancelled 已中止:案件结案/探针租约过期被回收。不是执行
	// 失败——没有结论产出,不计入失败统计(批9 僵尸探针事故引入:
	// 结案与失联回收需要与真失败区分,否则面板失败数失真)。
	ProbeCancelled ProbeTaskState = "cancelled"
)

// ErrProbeAlreadySettled 探针任务已被结案中止/租约回收(或已落地/不存在):
// 执行中的写回与中止竞态时,后到的结论必须丢弃,
// 不得复活 cancelled 行(批10 回归)。registry 返回,
// investigator 据此良性忽略——两包经本词汇交换,无包间依赖。
var ErrProbeAlreadySettled = errors.New("probe task already settled (cancelled or completed)")

// ProbeResult 是探针结论(与观测 Outcome 对齐,error 不可采)。
type ProbeResult string

const (
	// ProbeResultClean 探测放行。
	ProbeResultClean ProbeResult = "clean"
	// ProbeResultDegraded 探测降智。
	ProbeResultDegraded ProbeResult = "degraded"
	// ProbeResultError 探测传输失败。
	ProbeResultError ProbeResult = "error"
)

// ProbeTask 是一个取证任务(调查局队列行的领域投影)。
// 零依赖词汇:registry(存储)与 investigator(派发/执行)共用,
// 避免包间反向依赖。
type ProbeTask struct {
	Experiment         ProbeExperiment
	ID                 uint64
	CaseID             uint64
	Direction          ProbeDirection
	DefendantAccountID uint64
	// DefendantNodeID/DefendantEpoch are the comparison path for an account
	// differential task. The queue/API retain these field names as the task
	// target.
	DefendantNodeID uint64
	DefendantEpoch  uint64
	// BaselineNodeID/BaselineEpoch identify the original degraded path. The
	// task only requests the comparison path; baseline is used for verification.
	BaselineNodeID uint64
	BaselineEpoch  uint64
	JurorAccountID uint64
	// The control repeats the experiment with one variable changed. For an
	// account test it uses a healthy account on the comparison exit; for an
	// exit test it uses the same juror on an independent comparison exit.
	ControlAccountID uint64
	ControlNodeID    uint64
	ControlEpoch     uint64
}

// ProbeTaskResult 是探针结论的领域投影。
type ProbeTaskResult struct {
	ResourceCheck  *ResourceCheckReport
	Attempt        attemptmeta.Identity
	ControlAttempt attemptmeta.Identity
	Outcome        ProbeResult
	// VerifiedIPChange 差分方向必须为真:出口 IP 未证实的差分结论
	// 一律不可采(I8 粘性池同 IP 差分事故)。
	VerifiedIPChange bool
	// Detail 规则指纹/脱敏摘要(I24:不含 IP 明文/账号名/密钥)。
	Detail string
	// FailureKind is structured diagnostic evidence, never a quality vote.
	FailureKind string
	// PathKey is a hash of the observed exit addresses, used to deduplicate
	// different node records that lead to the same IP.
	PathKey         string
	ControlOutcome  ProbeResult
	ControlDetail   string
	ControlPathKey  string
	ControlVerified bool
}

// observationFromResult 结论转证据观测(source=probe)。
func ProbeObservation(task ProbeTask, result ProbeTaskResult, at time.Time) Observation {
	account := task.DefendantAccountID
	exit := EpochKey{NodeID: task.DefendantNodeID, Epoch: task.DefendantEpoch}
	if task.Direction == ProbeExitJury {
		// 陪审员结论的观测主体是陪审员(经被告出口的判定)。
		account = task.JurorAccountID
	}
	if result.Attempt.ID != "" {
		account = result.Attempt.AccountID
		exit = EpochKey{NodeID: result.Attempt.Path.NodeID, Epoch: result.Attempt.Path.Epoch}
	}
	outcome := OutcomeDelivered
	switch result.Outcome {
	case ProbeResultDegraded:
		outcome = OutcomeDegraded
	case ProbeResultError:
		outcome = OutcomeError
	}
	rule := result.Detail
	if rule == "" {
		rule = fmt.Sprintf("probe_%s", task.Direction)
	}
	eventID := ""
	if task.ID != 0 {
		eventID = fmt.Sprintf("probe/task/%d", task.ID)
	}
	return Observation{
		EventID: eventID,
		At:      at, AccountID: account, Exit: exit, Attempt: result.Attempt,
		Source: SourceProbe, Outcome: outcome, Rule: rule,
	}
}
