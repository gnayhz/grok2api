package gateway

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
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
	// GuardSignalWithhold: 有输出但缺少思考证据被判扣留。
	GuardSignalWithhold GuardSignal = "missing_thinking"
)

// guardSignalOrder 定义 UI 展示顺序(按拦截时机从早到晚)。
var guardSignalOrder = []GuardSignal{
	GuardSignalCreatedTimeout,
	GuardSignalEvidenceTimeout,
	GuardSignalEmptyStream,
	GuardSignalWithhold,
}

// guardExemptOrder 定义守卫豁免原因的展示顺序,与 quality_retry.go 中的
// QualityExempt* 常量一一对应。未知 token 按信号同约定丢弃——重命名不会
// 静默清零计数,缺失会立刻在快照里可见。
var guardExemptOrder = []string{
	QualityExemptDisabled,
	QualityExemptSkipInput,
	QualityExemptOperation,
	QualityExemptCompaction,
	QualityExemptProvider,
	QualityExemptModelScope,
	QualityExemptModelNoReasoning,
}

// GuardSignalStat 是一个特征自进程启动以来的累计观测。
type GuardSignalStat struct {
	Signal string `json:"signal"`
	// Triggered 该特征触发的总次数(每次尝试计一次,一个请求可多次)。
	Triggered int64 `json:"triggered"`
	// Requests 首个触发特征为该信号的请求数(每请求至多计一次)。
	Requests int64 `json:"requests"`
	// Rescued 最终交付完成且没有传输/协议错误的请求数；仅收到思考不计成功。
	Rescued int64 `json:"rescued"`
	// Failed 包含交付前拒绝以及响应头发送后的断流、取消。
	Failed   int64      `json:"failed"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// GuardExemptStat 是质量守卫某条豁免路径自进程启动以来放行的请求数。
// 豁免本身是设计内行为(协议无证据通道/模型不支持推理等),但必须可观测:
// "为什么这批降智请求没被拦"的第一反应应该是看这里。
type GuardExemptStat struct {
	Reason   string     `json:"reason"`
	Count    int64      `json:"count"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// GuardRetrialStat 汇总守卫触发后的恢复路径。
type GuardRetrialStat struct {
	// ExhaustedRejected 重试耗尽后 fail-closed 拒绝的请求数(耗尽策略
	// 只剩 fail-closed,G12;同号重试已废除,G14)。
	ExhaustedRejected int64 `json:"exhaustedRejected"`
}

// GuardEffectiveConfig 是守卫当前生效配置的只读投影。由
// GuardStatsSnapshot 每次读取时从当前快照配置重建,面板状态卡与外部
// 告警据此直接回答"守卫现在是否在场"——历史事故中该状态只能靠豁免
// 计数器事后反推。
type GuardEffectiveConfig struct {
	Enabled       bool      `json:"enabled"`
	MaxAttempts   int       `json:"maxAttempts"`
	OnExhausted   string    `json:"onExhausted"`
	GuardedModels []string  `json:"guardedModels,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// GuardStatsSnapshot 是管理端读取的完整快照。计数为进程本地,重启归零
// (与 routingStats 同生命周期语义)。
type GuardStatsSnapshot struct {
	Resources          responsebuffer.Snapshot       `json:"resources"`
	LocalProtection    selector.LocalProtectionStats `json:"localProtection"`
	Backlog            *GuardBacklogStats            `json:"backlog,omitempty"`
	BacklogUnavailable bool                          `json:"backlogUnavailable,omitempty"`
	Signals            []GuardSignalStat             `json:"signals"`
	// Exempts 按原因统计守卫未介入的请求(与 Signals 同生命周期语义)。
	Exempts []GuardExemptStat `json:"exempts"`
	Retrial GuardRetrialStat  `json:"retrial"`
	// Effective 守卫当前生效配置投影(读取时从快照配置重建);nil=尚未装配。
	Effective *GuardEffectiveConfig `json:"effective,omitempty"`
	// Since 统计起点(进程启动后首次记录)。
	Since *time.Time `json:"since,omitempty"`
}

// guardStatsCollector 在进程内存累计守卫特征观测。独立于 perfmetrics:
// 注册表每分钟被日志任务 CollectAndReset 清空,而管理 UI 需要跨清理周期
// 存活的累计快照(与 egress.routingStats 相同的取舍)。
type guardStatsCollector struct {
	mu      sync.Mutex
	signals map[GuardSignal]*GuardSignalStat
	exempts map[string]*GuardExemptStat
	retrial GuardRetrialStat
	since   time.Time
}

var guardStats = newGuardStatsCollector()

func newGuardStatsCollector() *guardStatsCollector {
	signals := make(map[GuardSignal]*GuardSignalStat, len(guardSignalOrder))
	for _, signal := range guardSignalOrder {
		signals[signal] = &GuardSignalStat{Signal: string(signal)}
	}
	exempts := make(map[string]*GuardExemptStat, len(guardExemptOrder))
	for _, reason := range guardExemptOrder {
		exempts[reason] = &GuardExemptStat{Reason: reason}
	}
	return &guardStatsCollector{signals: signals, exempts: exempts}
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

// recordExempt 记录一次守卫豁免(按原因 token)。未知 token 丢弃——
// 与信号同约定:常量重命名会让计数停在新键上,而不是静默归零旧键。
func (c *guardStatsCollector) recordExempt(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stat, ok := c.exempts[reason]
	if !ok {
		return
	}
	c.touch()
	stat.Count++
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

func (c *guardStatsCollector) recordExhausted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touch()
	c.retrial.ExhaustedRejected++
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
	exempts := make([]GuardExemptStat, 0, len(guardExemptOrder))
	for _, reason := range guardExemptOrder {
		if stat, ok := c.exempts[reason]; ok {
			copied := *stat
			if stat.LastSeen != nil {
				seen := *stat.LastSeen
				copied.LastSeen = &seen
			}
			exempts = append(exempts, copied)
		}
	}
	snapshot := GuardStatsSnapshot{Signals: signals, Exempts: exempts, Retrial: c.retrial}
	if !c.since.IsZero() {
		since := c.since
		snapshot.Since = &since
	}
	return snapshot
}

// processGuardStatsSnapshot 读取进程级守卫计数(无 Service 实例时的兜底投影)。
func processGuardStatsSnapshot() GuardStatsSnapshot { return guardStats.Snapshot() }

// GuardStatsSnapshot pairs process counters with this gateway's policy authority.
// Effective 投影每次读取都从当前快照配置(QualityRetryConfig)重建,
// 守卫配置热更由组合根的快照源即时反映。
func (s *Service) GuardStatsSnapshot() GuardStatsSnapshot {
	if s == nil {
		return processGuardStatsSnapshot()
	}
	snapshot := processGuardStatsSnapshot()
	snapshot.Resources = responsebuffer.ProcessSnapshot()
	cfg := s.QualityRetryConfig()
	if s.selector != nil {
		snapshot.LocalProtection = s.selector.ProtectionStats()
	}
	snapshot.Effective = &GuardEffectiveConfig{Enabled: cfg.Enabled, MaxAttempts: cfg.MaxAttempts,
		OnExhausted: cfg.OnExhausted, GuardedModels: cfg.GuardedModels}
	return snapshot
}

type GuardBacklogStats struct {
	InFlight        int64      `json:"inFlight"`
	Unconfirmed     int64      `json:"unconfirmed"`
	OldestExpiredAt *time.Time `json:"oldestExpiredAt,omitempty"`
	Pending         int64      `json:"pending"`
	Limit           int64      `json:"limit"`
	OldestAt        *time.Time `json:"oldestAt,omitempty"`
	Leased          int64      `json:"leased"`
	Retrying        int64      `json:"retrying"`
}
type GuardBacklogSource interface {
	GuardBacklog(context.Context) (GuardBacklogStats, error)
}

func (s *Service) GuardStatsSnapshotWithContext(ctx context.Context) GuardStatsSnapshot {
	if s == nil {
		return processGuardStatsSnapshot()
	}
	snapshot := s.GuardStatsSnapshot()
	if recorder := s.qualityEvents.Load(); recorder != nil {
		if source, ok := recorder.value.(GuardBacklogSource); ok {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			stats, err := source.GuardBacklog(ctx)
			if err != nil {
				snapshot.BacklogUnavailable = true
			} else {
				snapshot.Backlog = &stats
			}
		}
	}
	return snapshot
}
