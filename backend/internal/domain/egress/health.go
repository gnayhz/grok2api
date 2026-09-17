package egress

import "time"

type HealthObservationKind uint8

const (
	HealthSuccess HealthObservationKind = iota + 1
	HealthTransportFailure
	HealthAntiBotRejection
)

// HealthObservation describes a completed physical request against the binding
// captured when its lease was issued. Success is conditional on that health
// revision; it cannot erase a failure/quarantine established after acquisition.
// Consecutive failures can be combined without losing their count.
type HealthObservation struct {
	NodeID            uint64
	EncryptedProxyURL string
	BindingRevision   uint64
	ExpectedRevision  uint64
	Kind              HealthObservationKind
	Failures          int
	CooldownUntil     *time.Time // Minimum transport cooldown, e.g. confirmed dead exit.
	ObservedAt        time.Time
}

// HealthState is the runtime projection shared by the observation processor
// and storage. It intentionally carries no administrator display associations.
type HealthState struct {
	Revision      uint64
	Health        float64
	FailureCount  int
	CooldownUntil *time.Time
	LastError     string
}

func (n Node) HealthState() HealthState {
	return HealthState{Revision: n.HealthRevision, Health: n.Health, FailureCount: n.FailureCount, CooldownUntil: n.CooldownUntil, LastError: n.LastError}
}

func (s HealthState) ApplyTo(n Node) Node {
	n.HealthRevision, n.Health, n.FailureCount, n.CooldownUntil, n.LastError = s.Revision, s.Health, s.FailureCount, s.CooldownUntil, s.LastError
	return n
}

// RotatingEndpointHealth 是"旋转端点无共享健康惩罚"的唯一投影:池模式端点
// (服务商自动更换出口 IP,或账号模板每账号独立粘性出口)的出口 IP 不属于
// 端点自身,单个坏 IP 不代表端点坏。管理端列表展示与自动调度都按满健康
// 呈现;它只影响这一份投影视图,存储层与反馈链路上的真实健康状态不变。
// 调用方必须先按 IsPoolMode/IsPoolModeNode 判定为池模式端点。
func RotatingEndpointHealth(state HealthState) HealthState {
	state.Health, state.FailureCount, state.CooldownUntil, state.LastError = 1, 0, nil, ""
	return state
}

// CooldownBlocksScheduling 是"进行中的冷却是否必须把出口挡在调度之外"的
// 唯一判定,自动调度、池成员过滤与固定目标三条路径共用,不得各自拼条件。
//
// 豁免口径只有一处,即此函数:池模式端点(见 IsPoolMode/IsPoolModeNode)的
// 普通冷却来自服务商侧的瞬时出口 IP,换一个出口即失效,因此不阻断调度;
// 但出口 IP 质量隔离(LastErrorExitIPQuality)指向端点自身的降智问题,由
// 质量治理显式施加,对池模式端点同样生效——否则被隔离的降智出口会在隔离
// 期内继续作为固定目标或池成员承流,隔离形同虚设。
//
// poolMode 必须由 domain 的池模式判定给出;now 使用调用方快照的时钟。
// 未设置冷却或冷却已到期时恒不阻断。
func CooldownBlocksScheduling(cooldownUntil *time.Time, lastError string, poolMode bool, now time.Time) bool {
	return cooldownUntil != nil &&
		(!poolMode || lastError == LastErrorExitIPQuality) &&
		now.Before(*cooldownUntil)
}

// 健康阶梯的唯一数值源。relational/egress_health.go 的 SQL CASE 由这些
// 常量与 CooldownDuration 计算后作为参数注入，不得在 SQL 中另写一套数字。
const (
	// HealthDecayFactor 是每次失败后健康分的衰减系数。
	HealthDecayFactor = 0.7
	// HealthFloor 是健康分的下限。
	HealthFloor = 0.05
	// HealthSuccessStep 是每次成功后健康分的恢复步长（上限 1）。
	HealthSuccessStep = 0.1
)

// CooldownDuration 返回传输失败后的冷却时长：30s << min(failureCount-1, 4)。
// failureCount 为更新后的失败计数；SQL 侧按同一函数注入阶梯阈值。
func CooldownDuration(failureCount int) time.Duration {
	if failureCount < 1 {
		failureCount = 1
	}
	return 30 * time.Second * time.Duration(1<<min(failureCount-1, 4))
}

// Apply uses the same transition as storage. Revision checks reject an old
// success locally before it can affect routing; storage repeats the check for
// other replicas and configuration changes.
func (s HealthState) Apply(o HealthObservation) (HealthState, bool) {
	if o.Kind == HealthSuccess {
		if s.Revision != o.ExpectedRevision {
			return s, false
		}
		s.Health = min(1, s.Health+HealthSuccessStep)
		s.FailureCount = 0
		if s.LastError != LastErrorExitIPQuality {
			s.CooldownUntil, s.LastError = nil, ""
		}
		s.Revision++
		return s, true
	}
	count := max(1, o.Failures)
	for i := 0; i < min(count, 32); i++ {
		s.Health = max(HealthFloor, s.Health*HealthDecayFactor)
	}
	s.FailureCount += count
	s.Revision += uint64(count)
	if s.LastError == LastErrorExitIPQuality {
		return s, true
	}
	if o.Kind == HealthAntiBotRejection {
		// Rejection is not evidence of transport recovery. Preserve a prior
		// cooldown (and its reason), even when this request predates it.
		if s.CooldownUntil == nil {
			s.LastError = "anti-bot rejection"
		}
		return s, true
	}
	until := o.ObservedAt.Add(CooldownDuration(s.FailureCount))
	if o.CooldownUntil != nil && o.CooldownUntil.After(until) {
		until = *o.CooldownUntil
	}
	if s.CooldownUntil != nil && s.CooldownUntil.After(until) {
		until = *s.CooldownUntil
	}
	s.CooldownUntil, s.LastError = &until, LastErrorTransport
	return s, true
}

// MergeFailures combines observations for the same binding. A transport
// failure must survive a later anti-bot rejection in a coalesced queue; only
// transport timestamps contribute to backoff, and explicit cooldowns are floors.
// Both observations must describe failures, not recovery.
func (o HealthObservation) MergeFailures(previous HealthObservation) HealthObservation {
	o.Failures = max(1, o.Failures) + max(1, previous.Failures)
	if previous.Kind != HealthTransportFailure {
		return o
	}
	if o.Kind != HealthTransportFailure {
		o.Kind, o.ObservedAt, o.CooldownUntil = previous.Kind, previous.ObservedAt, previous.CooldownUntil
		return o
	}
	if previous.ObservedAt.After(o.ObservedAt) {
		o.ObservedAt = previous.ObservedAt
	}
	if previous.CooldownUntil != nil && (o.CooldownUntil == nil || previous.CooldownUntil.After(*o.CooldownUntil)) {
		o.CooldownUntil = previous.CooldownUntil
	}
	return o
}
