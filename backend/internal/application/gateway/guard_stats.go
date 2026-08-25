package gateway

import (
	"sync"
	"time"
)

// GuardSignal 命名实时路由守卫的一个可观测"防降智特征"。类型化常量跨
// 记录与展示两侧共享,未知值在收集器内被丢弃——重命名不会静默清零计数。
type GuardSignal string

const (
	// GuardSignalCreatedTimeout: 首事件截止(任何 SSE data 事件到达前超时)。
	GuardSignalCreatedTimeout GuardSignal = "created_timeout"
	// GuardSignalEvidenceTimeout: 零证据截止(静默期无思考证据且无输出)。
	GuardSignalEvidenceTimeout GuardSignal = "evidence_timeout"
	// GuardSignalEmptyStream: 流结束但零内容零推理。
	GuardSignalEmptyStream GuardSignal = "empty_stream"
	// GuardSignalHeaderBudget: 响应头预算早断。
	GuardSignalHeaderBudget GuardSignal = "header_budget"
	// GuardSignalWithhold: 有输出但缺少思考证据被判扣留。
	GuardSignalWithhold GuardSignal = "missing_thinking"
)

// guardSignalOrder 定义 UI 展示顺序(按拦截时机从早到晚)。
var guardSignalOrder = []GuardSignal{
	GuardSignalHeaderBudget,
	GuardSignalCreatedTimeout,
	GuardSignalEvidenceTimeout,
	GuardSignalEmptyStream,
	GuardSignalWithhold,
}

// GuardSignalStat 是一个特征自进程启动以来的累计观测。
type GuardSignalStat struct {
	Signal string `json:"signal"`
	// Triggered 该特征触发的总次数(每次尝试计一次,一个请求可多次)。
	Triggered int64 `json:"triggered"`
	// Requests 首个触发特征为该信号的请求数(每请求至多计一次)。
	Requests int64 `json:"requests"`
	// Rescued 这些请求最终成功交付(200)的数量。
	Rescued int64 `json:"rescued"`
	// Failed 这些请求最终失败(4xx/5xx/503)的数量。
	Failed   int64      `json:"failed"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// GuardRetrialStat 汇总守卫触发后的恢复路径。
type GuardRetrialStat struct {
	// SameAccountRetryUsed 同号补偿重试发起次数。
	SameAccountRetryUsed int64 `json:"sameAccountRetryUsed"`
	// SameAccountRetryRescued 其中最终成功交付的请求数。
	SameAccountRetryRescued int64 `json:"sameAccountRetryRescued"`
	// ExhaustedDeliverLast 重试耗尽后 fail-open 放行最后一次的请求数。
	ExhaustedDeliverLast int64 `json:"exhaustedDeliverLast"`
	// ExhaustedRejected 重试耗尽后 fail-closed 拒绝的请求数。
	ExhaustedRejected int64 `json:"exhaustedRejected"`
}

// GuardStatsSnapshot 是管理端读取的完整快照。计数为进程本地,重启归零
// (与 routingStats 同生命周期语义)。
type GuardStatsSnapshot struct {
	Signals []GuardSignalStat `json:"signals"`
	Retrial GuardRetrialStat  `json:"retrial"`
	// Canary 按结论统计换 IP 后 canary 一次性验证的执行结果。
	Canary map[string]int64 `json:"canary"`
	// Since 统计起点(进程启动后首次记录)。
	Since *time.Time `json:"since,omitempty"`
}

// guardStatsCollector 在进程内存累计守卫特征观测。独立于 perfmetrics:
// 注册表每分钟被日志任务 CollectAndReset 清空,而管理 UI 需要跨清理周期
// 存活的累计快照(与 egress.routingStats 相同的取舍)。
type guardStatsCollector struct {
	mu      sync.Mutex
	signals map[GuardSignal]*GuardSignalStat
	retrial GuardRetrialStat
	canary  map[string]int64
	since   time.Time
}

var guardStats = newGuardStatsCollector()

func newGuardStatsCollector() *guardStatsCollector {
	signals := make(map[GuardSignal]*GuardSignalStat, len(guardSignalOrder))
	for _, signal := range guardSignalOrder {
		signals[signal] = &GuardSignalStat{Signal: string(signal)}
	}
	return &guardStatsCollector{signals: signals, canary: make(map[string]int64)}
}

func (c *guardStatsCollector) touch() {
	if c.since.IsZero() {
		c.since = time.Now().UTC()
	}
}

// recordSignal 记录一次特征触发(每次尝试都计)。
func (c *guardStatsCollector) recordSignal(signal GuardSignal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stat, ok := c.signals[signal]
	if !ok {
		return // 未知信号按约定丢弃
	}
	c.touch()
	stat.Triggered++
	now := time.Now().UTC()
	stat.LastSeen = &now
}

// recordRequestSignal 记录请求的首个触发特征(每请求一次)。
func (c *guardStatsCollector) recordRequestSignal(signal GuardSignal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stat, ok := c.signals[signal]
	if !ok {
		return
	}
	c.touch()
	stat.Requests++
}

// recordOutcome 记录带首个触发特征请求的最终结局。
func (c *guardStatsCollector) recordOutcome(signal GuardSignal, rescued bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stat, ok := c.signals[signal]
	if !ok {
		return
	}
	if rescued {
		stat.Rescued++
	} else {
		stat.Failed++
	}
}

func (c *guardStatsCollector) recordSameAccountRetry() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touch()
	c.retrial.SameAccountRetryUsed++
}

func (c *guardStatsCollector) recordSameAccountRescued() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retrial.SameAccountRetryRescued++
}

func (c *guardStatsCollector) recordExhausted(deliverLast bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touch()
	if deliverLast {
		c.retrial.ExhaustedDeliverLast++
	} else {
		c.retrial.ExhaustedRejected++
	}
}

func (c *guardStatsCollector) recordCanary(outcome string) {
	if outcome == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touch()
	c.canary[outcome]++
}

// Snapshot 返回稳定排序的副本,保证 UI 轮询行序不跳动。
func (c *guardStatsCollector) Snapshot() GuardStatsSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	signals := make([]GuardSignalStat, 0, len(c.signals))
	for _, signal := range guardSignalOrder {
		if stat, ok := c.signals[signal]; ok {
			copied := *stat
			if stat.LastSeen != nil {
				seen := *stat.LastSeen
				copied.LastSeen = &seen
			}
			signals = append(signals, copied)
		}
	}
	// signals 已按 guardSignalOrder 顺序构造,无需再排序。
	canary := make(map[string]int64, len(c.canary))
	for outcome, count := range c.canary {
		canary[outcome] = count
	}
	snapshot := GuardStatsSnapshot{Signals: signals, Retrial: c.retrial, Canary: canary}
	if !c.since.IsZero() {
		since := c.since
		snapshot.Since = &since
	}
	return snapshot
}

// GuardStatsSnapshotForAPI 暴露给只读管理端点。
func GuardStatsSnapshotForAPI() GuardStatsSnapshot { return guardStats.Snapshot() }
