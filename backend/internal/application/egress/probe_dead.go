package egress

import (
	"context"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// 死出口确认(探活连续失败判定)。设计要点:
//
// 单次双族(IPv4+IPv6)探活失败不构成死出口证据——探活本身有抖动(瞬时
// 网络毛刺、回显服务波动), "显示不通、重试又通"是常态。只有连续两次
// 双族失败才升级为死出口事件:
//
//   - 第一次失败后 45s 自动补测一次(不等人点重试), 两次都失败才标记;
//     补测成功则计数归零, 节点从未被排除。
//   - 标记 = transport 冷却 10 分钟(与请求级传输失败冷却上限一致): 节点
//     立即退出池/自动调度; 下一次健康探活自动清除冷却(repository 健康分支
//     按 last_error=transport 清状态), 隧道恢复即自动回池, 无需人工。
//   - 配置了换 IP webhook 的节点同时入轮换队列: 隧道卡死正是 restart 的
//     适应症, 复用轮换的全部护栏(MinNodeInterval/全局限流/尝试上限)与
//     验证闭环(settle → 探活 → 双族 IP 校验)。
//
// 与质量守卫的边界: 质量隔离中的节点(LastError=exit_ip_quality)跳过——
// 它由质量闭环管理, transport 冷却会覆盖其 24h 隔离语义。代理池模式
// (旋转端点/账号模板)同样跳过, 与调度豁免口径一致。
var probeDeadConfirmDelay = 45 * time.Second

const (
	probeDeadCooldown = 10 * time.Minute
	probeDeadWindow   = 30 * time.Minute
)

type probeDeadObservation struct {
	binding    string
	revision   uint64
	count      int
	at         time.Time
	confirming bool
}

// probeDeadTracker 持有死出口确认的可变状态: 观察表、互斥与容量上限。
// Service 只经组件方法访问; 状态/锁归一个组件所有。
type probeDeadTracker struct {
	mu  sync.Mutex
	obs map[uint64]probeDeadObservation
}

// observe 记录一次双族失败并返回 (fresh, act, scheduleConfirm)。
func (t *probeDeadTracker) observe(node domain.Node, now time.Time) (fresh, act, scheduleConfirm bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.obs == nil {
		t.obs = make(map[uint64]probeDeadObservation)
	}
	entry, ok := t.obs[node.ID]
	if !ok || now.Sub(entry.at) > probeDeadWindow || entry.binding != node.EncryptedProxyURL || entry.revision != node.BindingRevision {
		entry = probeDeadObservation{}
	}
	entry.binding, entry.revision = node.EncryptedProxyURL, node.BindingRevision
	entry.at = now
	entry.count++
	scheduleConfirm = entry.count == 1 && !entry.confirming
	if scheduleConfirm {
		entry.confirming = true
	}
	if entry.count >= 2 {
		entry.confirming = false
	}
	if len(t.obs) >= 4096 {
		for id, value := range t.obs {
			if now.Sub(value.at) > probeDeadWindow {
				delete(t.obs, id)
			}
		}
		if len(t.obs) >= 4096 {
			for id := range t.obs {
				delete(t.obs, id)
				break
			}
		}
	}
	t.obs[node.ID] = entry
	return entry.count == 2, entry.count >= 2, scheduleConfirm
}

func (t *probeDeadTracker) clear(nodeID uint64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.obs, nodeID)
	t.mu.Unlock()
}

func (t *probeDeadTracker) clearConfirming(nodeID uint64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	entry := t.obs[nodeID]
	entry.confirming = false
	t.obs[nodeID] = entry
	t.mu.Unlock()
}

// observeProbeResult 是所有探活结果的统一漏斗(手动单测/批量测试/定时
// 巡检/确认补测都经此)。waitNodeHealthy 的密集重试除外(testNode 的
// observe=false): 那不是独立健康观测, 计入会把轮换等待期污染成"确认"。
func (s *Service) observeProbeResult(node domain.Node, result domain.ProbeResult) {
	if s == nil || node.ID == 0 {
		return
	}
	dead := result.IPv4.Status == domain.ProbeStatusUnhealthy && result.IPv6.Status == domain.ProbeStatusUnhealthy
	now := time.Now().UTC()
	if !dead {
		s.probeDead.clear(node.ID)
		return
	}
	if s.probeDead == nil {
		s.probeDead = &probeDeadTracker{}
	}
	fresh, act, scheduleConfirm := s.probeDead.observe(node, now)
	// 捕获当前确认延迟：confirmProbeLater 是异步 goroutine，事后读取
	// 可变包级变量会与测试的恢复写入构成数据竞争（race detector 已
	// 抓获）。在调度点取快照即可根治。
	confirmDelay := probeDeadConfirmDelay
	if scheduleConfirm {
		if !s.background().after(confirmDelay, func(ctx context.Context) { s.confirmProbe(ctx, node) }) {
			s.probeDead.clearConfirming(node.ID)
		}
	}
	if act {
		s.markProbeDead(node, fresh)
	}
}

// confirmProbeLater 在确认延迟后补测一次: 抖动型失败在此窗口内自愈则计数
// 归零(节点从未被标记), 仍然双族失败则计数到达阈值并触发标记。
func (s *Service) confirmProbe(ctx context.Context, node domain.Node) {
	latest, err := s.repository.GetEgressNode(ctx, node.ID)
	if err != nil || latest.EncryptedProxyURL != node.EncryptedProxyURL || latest.BindingRevision != node.BindingRevision {
		return
	}
	_, _ = s.testNode(ctx, node.ID, true)
}

// markProbeDead 对确认死出口的节点施加 transport 冷却并(配置了 webhook
// 时)入轮换队列。fresh 表示新的死出口事件(计数恰好到达阈值): 重置轮换
// 尝试计数, 让每个独立事件都拥有完整的 MaxAttemptsPerQuarantine 预算;
// LastRotatedAt 由 SQL COALESCE 保留, MinNodeInterval 护栏不被击穿。
func (s *Service) markProbeDead(observed domain.Node, fresh bool) {
	if s == nil || s.repository == nil {
		return
	}
	nodeID := observed.ID
	s.mu.RLock()
	quarantiner := s.qualityQuarantiner
	rotCfg := s.rotationCfg
	cipher := s.cipher
	s.mu.RUnlock()
	if quarantiner == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.background().ctx, 30*time.Second)
	defer cancel()
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		s.rotationLog().Warn("egress_probe_dead_read_failed", "node_id", nodeID, "error", err.Error())
		return
	}
	if node.EncryptedProxyURL != observed.EncryptedProxyURL || node.BindingRevision != observed.BindingRevision {
		return
	}
	now := time.Now().UTC()
	if !node.Enabled || node.EncryptedProxyURL == "" {
		return
	}
	if node.CooldownUntil != nil && node.CooldownUntil.After(now) && node.LastError == domain.LastErrorExitIPQuality {
		return
	}
	// 池模式判定委托 domain.IsPoolMode 单源策略(节点池标志 || 账号模板
	// 代理);不得在调用方手拼同一条件——三处早退曾各自维护平行副本。
	if node.ProxyPool {
		return
	}
	if cipher != nil {
		if proxyURL, decryptErr := cipher.Decrypt(node.EncryptedProxyURL); decryptErr == nil && domain.IsPoolMode(false, domain.IsAccountTemplateProxy(proxyURL)) {
			return
		}
	}
	cooldown := func() error {
		if bound, ok := quarantiner.(interface {
			CooldownBindingForProbeFailure(context.Context, domain.Node, time.Time) error
		}); ok {
			return bound.CooldownBindingForProbeFailure(ctx, observed, now.Add(probeDeadCooldown))
		}
		return quarantiner.CooldownNodeForProbeFailure(ctx, nodeID, now.Add(probeDeadCooldown))
	}
	if err := cooldown(); err != nil {
		s.rotationLog().Warn("egress_probe_dead_cooldown_failed", "node_id", nodeID, "error", err.Error())
		return
	}
	perfmetrics.Default.Inc("egress_probe_dead_total", perfmetrics.Labels{Subsystem: "egress", Operation: "probe_dead", Outcome: "marked"})
	s.rotationLog().Warn("egress_probe_dead_marked", "node_id", nodeID, "node", node.Name, "cooldown", probeDeadCooldown.String())
	if node.EncryptedRotationURL == "" || !node.RotationEnabled || !rotCfg.Enabled {
		return
	}
	budget := node.RotationAttempts
	if fresh {
		s.recordRotationState(ctx, nodeID, 0, "", false)
		budget = 0
	}
	if budget < rotCfg.MaxAttemptsPerQuarantine {
		s.enqueueRotation(nodeID)
		s.rotationLog().Info("egress_probe_dead_rotation_enqueued", "node_id", nodeID, "node", node.Name, "fresh", fresh)
	}
}
