package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// RotationConfig controls automatic exit-IP rotation (restart the tunnel
// service behind a node via its webhook) after an exit-IP quality quarantine.
type RotationConfig struct {
	Enabled                  bool
	MaxAttemptsPerQuarantine int
	MinNodeInterval          time.Duration
	MaxGlobalPerHour         int
	WebhookTimeout           time.Duration
	WebhookRetries           int
	SettleDelay              time.Duration
	ProbeTimeout             time.Duration
	ProbeInterval            time.Duration
}

func (c RotationConfig) normalized() RotationConfig {
	if c.MaxAttemptsPerQuarantine <= 0 {
		c.MaxAttemptsPerQuarantine = 3
	}
	if c.MinNodeInterval <= 0 {
		c.MinNodeInterval = 3 * time.Minute
	}
	if c.MaxGlobalPerHour <= 0 {
		c.MaxGlobalPerHour = 6
	}
	if c.WebhookTimeout <= 0 {
		c.WebhookTimeout = 15 * time.Second
	}
	if c.WebhookRetries < 0 {
		c.WebhookRetries = 2
	}
	if c.SettleDelay < 0 {
		c.SettleDelay = 20 * time.Second
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 2 * time.Minute
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = 5 * time.Second
	}
	return c
}

type rotationScheduler struct {
	timers map[uint64]*time.Timer
	closed bool
	mu     sync.Mutex
	set    map[uint64]struct{}
	queue  []uint64
	wake   chan struct{}

	// epoch 在每次启用状态变更时递增;requeueAfter 的挂起定时器携带其
	// 创建时的 epoch,到点后仅当 epoch 未变才回流——"禁用丢弃全部排队
	// 工作"的契约否则会被未到期的定时器绕过(限速/最小间隔的重排到点后
	// 把节点送回已禁用的队列)。
	epoch   uint64
	recover bool
}

// RotationConfig returns the current rotation scheduler config, for
// callers adjusting a single field and reinstalling the rest unchanged.
func (s *Service) RotationConfig() RotationConfig {
	if s == nil {
		return RotationConfig{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rotationCfg
}

// SetRotationConfig installs or updates the rotation scheduler. Disabled
// config drops any queued work.
func (s *Service) SetRotationConfig(cfg RotationConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg = cfg.normalized()
	if s.rotation == nil {
		s.rotation = &rotationScheduler{set: make(map[uint64]struct{}), wake: make(chan struct{}, 1)}
	} else if !cfg.Enabled {
		s.rotation.mu.Lock()
		s.rotation.clearLocked()
		s.rotation.mu.Unlock()
	}
	if cfg.Enabled && !s.rotationCfg.Enabled {
		s.rotation.mu.Lock()
		s.rotation.recover = true
		s.rotation.mu.Unlock()
		select {
		case s.rotation.wake <- struct{}{}:
		default:
		}
	}
	s.rotationCfg = cfg
}

// SetRotationLogger installs the rotation scheduler logger.
func (s *Service) SetRotationLogger(value *slog.Logger) {
	if s == nil || value == nil {
		return
	}
	s.mu.Lock()
	s.rotationLogger = value
	s.mu.Unlock()
}

// SetRotationSuccessObserver 安装轮换成功后的单节点出口身份观测回调。
// 组合根把质量执行所的 ObserveNodeExit 接进来:webhook 已验证出口身份
// 变化,新身份落库与旧限制释放不必等下一个 5 分钟检测节拍(闭环尾延
// 从≤5m 降到秒级)。回调失败只由回调方记录,不影响轮换结果。
func (s *Service) SetRotationSuccessObserver(fn func(context.Context, uint64)) {
	if s == nil || fn == nil {
		return
	}
	s.mu.Lock()
	s.rotationObserver = fn
	s.mu.Unlock()
}

// notifyRotationSuccess 在成功记账后驱动观测回调。使用脱离轮换预算的
// 短超时上下文,回调不得拖住 worker 消费下一个节点;回调 panic 由外层
// batch.Do 隔离,不会击穿进程。
func (s *Service) notifyRotationSuccess(ctx context.Context, nodeID uint64) {
	s.mu.RLock()
	fn := s.rotationObserver
	s.mu.RUnlock()
	if fn == nil {
		return
	}
	observeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	fn(observeCtx, nodeID)
}

// RotateNode enqueues one node for immediate rotation (manual trigger).
// 手动轮换重开一个完整周期:尝试账本清零(attempts=0、错误清空),使耗尽
// (attempts>=max)或间隔未到的节点也能立即重新轮换——EXIT-IP-GUARD 承诺
// 耗尽后"需人工介入",这个入口就是人工介入本身。护栏保留:全局每小时上限
// (maxGlobalPerHour)与节点启用/webhook 配置检查不变,防手抖连点打爆隧道。
// 入队前先做与 processRotation 相同的跳过路径校验,把真实原因返回给操作者,
// 而不是"已排队"之后在 worker 里静默丢弃。
func (s *Service) RotateNode(ctx context.Context, nodeID uint64) error {
	return s.queueRotation(ctx, nodeID, true)
}

// RotateNodeAutomatically preserves the failed-attempt budget across repeated sweeps.
func (s *Service) RotateNodeAutomatically(ctx context.Context, nodeID uint64) error {
	return s.queueRotation(ctx, nodeID, false)
}

// ErrRotationQueueFull is admission feedback shared by all rotation callers.
var ErrRotationQueueFull = errors.New("rotation queue is full")

func (s *Service) queueRotation(ctx context.Context, nodeID uint64, manual bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if s == nil || s.repository == nil {
		return ErrOperationsUnavailable
	}
	release, acquired, err := s.acquireRotationOwner(ctx, nodeID)
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("node rotation already in progress")
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		// 与其他节点路由一致:缺失节点归一为应用层 ErrNotFound(404),
		// 而不是把 repository 错误透传成 500。
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.mu.RLock()
	enabled := s.rotationCfg.Enabled
	maxAttempts := s.rotationCfg.MaxAttemptsPerQuarantine
	s.mu.RUnlock()
	if !enabled {
		return errors.New("出口轮换未启用")
	}
	if !node.Enabled {
		return errors.New("节点已停用")
	}
	if strings.TrimSpace(node.EncryptedRotationURL) == "" {
		return errors.New("节点未配置换 IP webhook")
	}
	if !node.RotationEnabled {
		return errors.New("该节点的换 IP 轮换已关闭")
	}
	if !manual && node.RotationAttempts >= maxAttempts {
		return errors.New("rotation attempts exhausted; operator review required")
	}
	// 重开周期:清尝试账本。LastRotatedAt 不动——MinNodeInterval 仍按上次
	// 真实换 IP 时间计算,自动轮换的防重启风暴护栏对手动触发同样生效;
	// 真正的立即执行由 worker 的 requeueAfter 到点驱动,全局限速兜底。
	if manual && (node.RotationAttempts > 0 || node.LastRotationError != "") {
		if err := s.recordRotationState(ctx, node, 0, "", false); err != nil {
			return err
		}
	}
	// Release the reset lease before waking a worker that needs the same key.
	release()
	release = nil
	return s.enqueueRotation(nodeID)
}

func (s *Service) enqueueRotation(nodeID uint64) error {
	if s == nil || nodeID == 0 {
		return ErrOperationsUnavailable
	}
	s.mu.RLock()
	rotation, enabled := s.rotation, s.rotationCfg.Enabled
	s.mu.RUnlock()
	if rotation == nil || !enabled {
		return ErrOperationsUnavailable
	}
	rotation.mu.Lock()
	if rotation.closed {
		rotation.mu.Unlock()
		return ErrOperationsUnavailable
	}
	if _, queued := rotation.set[nodeID]; queued {
		rotation.mu.Unlock()
		return nil
	}
	if len(rotation.queue) >= 4096 {
		rotation.mu.Unlock()
		return ErrRotationQueueFull
	}
	rotation.set[nodeID] = struct{}{}
	rotation.queue = append(rotation.queue, nodeID)
	rotation.mu.Unlock()
	select {
	case rotation.wake <- struct{}{}:
	default:
	}
	return nil
}

func (r *rotationScheduler) next() (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 {
		return 0, false
	}
	nodeID := r.queue[0]
	r.queue = r.queue[1:]
	delete(r.set, nodeID)
	return nodeID, true
}

func (r *rotationScheduler) requeue(nodeID uint64) {
	r.mu.Lock()
	if !r.closed && len(r.queue) < 4096 {
		if _, queued := r.set[nodeID]; !queued {
			r.set[nodeID] = struct{}{}
			r.queue = append(r.queue, nodeID)
		}
	}
	r.mu.Unlock()
}

// requeueAfter 在 delay 到点后把节点重排队尾并唤醒 worker。worker 在等待
// 期间保持空闲可消费其他节点——替代此前的原地 select 睡眠(单 worker 会被
// 一个未到期节点阻塞最长 10 分钟, 且全局限速命中时曾空转等待近 1 小时)。
func (r *rotationScheduler) requeueAfter(nodeID uint64, delay time.Duration) {
	if delay <= 0 {
		r.requeue(nodeID)
		return
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	r.mu.Lock()
	if r.closed || len(r.timers) >= 1024 {
		r.mu.Unlock()
		return
	}
	if r.timers == nil {
		r.timers = make(map[uint64]*time.Timer)
	}
	if previous := r.timers[nodeID]; previous != nil {
		previous.Stop()
	}
	epoch := r.epoch
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		r.mu.Lock()
		if r.timers[nodeID] != timer {
			r.mu.Unlock()
			return
		}
		delete(r.timers, nodeID)
		if r.closed || r.epoch != epoch {
			r.mu.Unlock()
			return
		}
		if !r.closed && len(r.queue) < 4096 {
			if _, queued := r.set[nodeID]; !queued {
				r.set[nodeID] = struct{}{}
				r.queue = append(r.queue, nodeID)
			}
		}
		r.mu.Unlock()
		select {
		case r.wake <- struct{}{}:
		default:
		}
	})
	r.timers[nodeID] = timer
	r.mu.Unlock()
}

// SetRotationCoordination installs the shared runtime authority before workers start.
func (s *Service) SetRotationCoordination(lock repository.DistributedLock, rate repository.RollingRateLimiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotationLock, s.rotationRate = lock, rate
}

const rotationWorkBudget = 4 * time.Minute
const rotationOwnershipTTL = rotationWorkBudget + 30*time.Second

func (s *Service) acquireRotationOwner(ctx context.Context, nodeID uint64) (func(), bool, error) {
	s.mu.RLock()
	lock := s.rotationLock
	s.mu.RUnlock()
	if lock == nil {
		return nil, false, errors.New("rotation coordination unavailable")
	}
	return lock.Acquire(ctx, fmt.Sprintf("egress:rotation:%d", nodeID), rotationOwnershipTTL)
}

// RunRotationWorker drains the rotation queue until ctx ends. Exactly one
// worker per process; each execution also owns a shared node lease.
// Attempt reservation precedes the webhook and survives a worker crash.
// 同步运行消费循环(由调用方 goroutine 托管, Run 的 WaitGroup 因此能等待真实
// worker 退出, 关闭顺序不再与 DB 关闭竞争); processRotation 经 batch.Do 隔离
// panic——轮换链路(webhook/解密/探测/canary 推理)任一 panic 不得击穿进程。
func (s *Service) RunRotationWorker(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.RLock()
	rotation := s.rotation
	s.mu.RUnlock()
	if rotation == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-rotation.wake:
		}
		rotation.mu.Lock()
		recover := rotation.recover
		rotation.recover = false
		rotation.mu.Unlock()
		if recover {
			s.recoverPendingRotations(ctx)
		}
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			nodeID, ok := rotation.next()
			if !ok {
				break
			}
			if err := batch.Do(ctx, func(taskCtx context.Context) error {
				s.processRotation(taskCtx, nodeID)
				return nil
			}); err != nil {
				var panicErr *batch.PanicError
				if errors.As(err, &panicErr) {
					s.rotationLog().Error("egress_rotation_panic", "node_id", nodeID, "panic", panicErr.Value, "stack", string(panicErr.Stack))
				}
			}
		}
	}
}

func (s *Service) rotationLog() *slog.Logger {
	if s == nil {
		return slog.Default()
	}
	s.mu.RLock()
	logger := s.rotationLogger
	s.mu.RUnlock()
	if logger == nil {
		return s.qualityLog()
	}
	return logger
}

// processRotation performs one full rotation cycle for a node: webhook,
// settle, probe, exit-IP check, canary verify, admit or retry.
func (s *Service) processRotation(ctx context.Context, nodeID uint64) {
	ctx, cancel := context.WithTimeout(ctx, rotationWorkBudget)
	defer cancel()
	logger := s.rotationLog()
	release, acquired, err := s.acquireRotationOwner(ctx, nodeID)
	if err != nil {
		logger.Warn("egress_rotation_coordination_failed", "node_id", nodeID, "error", err)
		return
	}
	if !acquired {
		return
	}
	defer release()
	s.mu.RLock()
	cfg := s.rotationCfg
	quarantiner := s.qualityQuarantiner
	cipher := s.cipher
	rate := s.rotationRate
	rotation := s.rotation
	s.mu.RUnlock()
	if quarantiner == nil || !cfg.Enabled || rate == nil {
		return
	}
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		logger.Warn("egress_rotation_read_failed", "node_id", nodeID, "error", err.Error())
		return
	}
	if !node.Enabled {
		return
	}
	if strings.TrimSpace(node.EncryptedRotationURL) == "" {
		// 无换 IP Webhook:外部代理池/家庭宽带型固定出口——出口不受本系统
		// 控制,IP 漂移的解禁检测由质量层执行所的 epoch 轮询承担(读取探活
		// 落库的 exit_ip/last_probed_at),轮换 worker 对这类节点无事可做。
		return
	}
	if !node.RotationEnabled {
		s.recordRotationState(ctx, node, node.RotationAttempts, "rotation disabled for this node", false)
		logger.Info("egress_rotation_skipped", "node_id", nodeID, "node", node.Name, "reason", "rotation disabled")
		return
	}
	rotationURL, err := cipher.Decrypt(node.EncryptedRotationURL)
	if err != nil {
		s.recordRotationState(ctx, node, node.RotationAttempts, "decrypt rotation url: "+err.Error(), false)
		logger.Warn("egress_rotation_decrypt_failed", "node_id", nodeID, "error", err.Error())
		return
	}
	if node.RotationAttempts >= cfg.MaxAttemptsPerQuarantine {
		logger.Warn("egress_rotation_exhausted", "node_id", nodeID, "node", node.Name, "attempts", node.RotationAttempts, "max", cfg.MaxAttemptsPerQuarantine)
		perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "exhausted"})
		s.recordRotationState(ctx, node, node.RotationAttempts, "rotation attempts exhausted", false)
		return
	}
	// 未到 MinNodeInterval:重排队尾让 worker 立即处理下一个到期节点, 而不是
	// 原地睡眠阻塞整个队列(此前一次等待最长 10 分钟, 其间其他节点的隔离
	// 轮换全部停滞)。
	if node.LastRotatedAt != nil {
		if wait := cfg.MinNodeInterval - time.Since(*node.LastRotatedAt); wait > 0 {
			s.recordRotationState(ctx, node, node.RotationAttempts, "min interval not elapsed", false)
			if rotation != nil {
				rotation.requeueAfter(nodeID, wait)
			}
			return
		}
	}
	allowed, wait, err := rate.AllowRolling(ctx, "egress:rotation:global", cfg.MaxGlobalPerHour, time.Hour)
	if err != nil {
		logger.Warn("egress_rotation_rate_failed", "node_id", nodeID, "error", err)
		return
	}
	if !allowed {
		logger.Info("egress_rotation_rate_limited", "node_id", nodeID, "retry_in", wait.Round(time.Second).String())
		if rotation != nil {
			rotation.requeueAfter(nodeID, wait)
		}
		return
	}
	// Reserve an attempt durably before external effects. A timeout, crash or
	// ambiguous webhook response consumes the same bounded automatic budget.
	node.RotationAttempts++
	if err := s.recordRotationState(ctx, node, node.RotationAttempts, "rotation in progress", true); err != nil {
		return
	}
	if err := s.callRotationWebhook(ctx, rotationURL, cfg); err != nil {
		s.failRotation(ctx, nodeID, &node, cfg, "webhook: "+err.Error(), logger)
		return
	}
	if cfg.SettleDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.SettleDelay):
		}
	}
	probe, err := s.waitNodeHealthy(ctx, nodeID, cfg)
	if err != nil {
		logger.Warn("egress_rotation_probe_error", "node_id", nodeID, "error", err.Error())
		s.failRotation(ctx, nodeID, &node, cfg, "probe: "+err.Error(), logger)
		return
	}
	if probe.Status != domain.ProbeStatusHealthy {
		s.failRotation(ctx, nodeID, &node, cfg, "probe unhealthy: "+probe.Error, logger)
		return
	}
	// 文档(EXIT-IP-GUARD §5)的端到端验证依赖此事件:webhook 已实际调用、
	// 即将进入等待与探活阶段。此前全链路没有该事件, 按文档 grep 验收必失败。
	logger.Info("egress_rotation_triggered", "node_id", nodeID, "node", node.Name)
	if node.ExitIP != "" && !exitIPRotationChanged(node, probe) {
		s.failRotation(ctx, nodeID, &node, cfg, "exit ip unchanged after rotation", logger)
		return
	}
	// The observed probe and final bookkeeping are separate facts. Only a
	// completion accepted for this binding may publish rotation success.
	if err := s.recordRotationState(ctx, node, 0, "", true); err != nil {
		return
	}
	fields := []any{"node_id", nodeID, "node", node.Name, "exit_ip", probe.ExitIP, "exit_ip_v6", probe.IPv6.ExitIP}
	if node.LastError == domain.LastErrorTransport {
		fields = append(fields, "reason", "probe_dead_recovered")
	}
	logger.Info("egress_rotation_succeeded", fields...)
	perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "succeeded"})
	s.notifyRotationSuccess(ctx, nodeID)
}

// failRotation records one failed rotation attempt and re-enqueues when the
// attempt budget allows another try.
func (s *Service) failRotation(ctx context.Context, nodeID uint64, node *domain.Node, cfg RotationConfig, reason string, logger *slog.Logger) {
	perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "failed"})
	attempts := node.RotationAttempts
	s.recordRotationState(ctx, *node, attempts, reason, false)
	logger.Warn("egress_rotation_failed", "node_id", nodeID, "attempt", attempts, "max", cfg.MaxAttemptsPerQuarantine, "reason", reason)
	if attempts < cfg.MaxAttemptsPerQuarantine {
		if err := s.enqueueRotation(nodeID); err != nil {
			logger.Warn("egress_rotation_requeue_failed", "node_id", nodeID, "error", err)
		}
	}
}

// recordRotationState records reservation or outcome. An attempted webhook
// advances LastRotatedAt before dispatch, even if its response is ambiguous.
func (s *Service) recordRotationState(ctx context.Context, binding domain.Node, attempts int, lastError string, rotated bool) error {
	var rotatedAt *time.Time
	if rotated {
		now := time.Now().UTC()
		rotatedAt = &now
	}
	lastError = truncString(lastError, 512)
	var err error
	if store, ok := s.repository.(rotationStateRepository); ok {
		err = store.UpdateEgressNodeRotationStateForBinding(ctx, binding, rotatedAt, attempts, lastError)
	} else {
		err = errors.New("rotation state repository unavailable")
	}
	if err != nil {
		s.rotationLog().Warn("egress_rotation_state_failed", "node_id", binding.ID, "error", err)
	}
	return err
}

type rotationStateRepository interface {
	UpdateEgressNodeRotationStateForBinding(ctx context.Context, binding domain.Node, lastRotatedAt *time.Time, attempts int, lastError string) error
}

// WebhookExecutor 是轮换 webhook 投递的消费方端口；传输实现位于 infra。
type WebhookExecutor interface {
	Notify(ctx context.Context, url string, timeout time.Duration, retries int) error
}

func (s *Service) SetSubscriptionFetcher(fetcher SubscriptionFetcher) {
	s.mu.Lock()
	s.subscriptionFetcher = fetcher
	s.mu.Unlock()
}

func (s *Service) SetWebhookExecutor(executor WebhookExecutor) {
	s.mu.Lock()
	s.webhookExecutor = executor
	s.mu.Unlock()
}

func (s *Service) callRotationWebhook(ctx context.Context, rotationURL string, cfg RotationConfig) error {
	s.mu.RLock()
	executor := s.webhookExecutor
	s.mu.RUnlock()
	if executor == nil {
		// 未装配传输端口与订阅拉取同一合同:不为缺依赖自建网络客户端。
		return errors.New("轮换 webhook 传输组件未装配")
	}
	return executor.Notify(ctx, rotationURL, cfg.WebhookTimeout, cfg.WebhookRetries)
}

// exitIPRotationChanged reports whether the node's exit identity actually
// changed after a rotation, comparing per address family (IPv4 vs IPv4,
// IPv6 vs IPv6). Some tunnel images (MicroWARP in particular) re-dial with a
// stable IPv4 while the IPv6 egress rotates every restart, so the legacy
// aggregate comparison (which prefers IPv4) would flag every rotation of
// such nodes as "unchanged" and exhaust them into permanent quarantine.
// Any family whose current ExitIP differs from its recorded predecessor
// counts as rotated. When neither family has a comparable history (legacy
// nodes carrying only the aggregate ExitIP), fall back to the aggregate
// comparison so the anti-fake-webhook guarantee is preserved.
func exitIPRotationChanged(node domain.Node, probe domain.ProbeResult) bool {
	changed := false
	comparable := false
	for _, family := range [][2]string{
		{node.IPv4Probe.ExitIP, probe.IPv4.ExitIP},
		{node.IPv6Probe.ExitIP, probe.IPv6.ExitIP},
	} {
		previous, current := family[0], family[1]
		if previous == "" || current == "" {
			continue
		}
		comparable = true
		if current != previous {
			changed = true
		}
	}
	if comparable {
		return changed
	}
	return probe.ExitIP != node.ExitIP
}

func (s *Service) waitNodeHealthy(ctx context.Context, nodeID uint64, cfg RotationConfig) (domain.ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout)
	defer cancel()
	var last domain.ProbeResult
	for {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		result, err := s.testNode(ctx, nodeID, false)
		if err != nil {
			var executionErr *domain.ProbeExecutionError
			if errors.As(err, &executionErr) && last.Status != "" {
				// An interrupted observation cannot replace the last completed
				// one. Still return the operation error so rotation cannot treat
				// the retained result as fresh confirmation.
				return last, err
			}
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if result.Status == domain.ProbeStatusHealthy {
			return result, nil
		}
		last = result
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(cfg.ProbeInterval):
		}
	}
}

func truncString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// recoverPendingRotations 把"重启前已隔离但轮换未完成"的节点重新入队。
// 轮换队列是进程内状态, 而隔离(cooldown_until + last_error=exit_ip_quality)
// 持久在库:若不在 worker 启动时恢复, 进程在隔离与轮换之间重启会让坏出口
// 静默滞留整个隔离周期——且隔离中的节点不承流, 不会再产生触发重新入队的
// 降智事件; 冷却到期后未经验证直接回池。入队本身幂等:processRotation 的
// 共享节点租约、调用前尝试账本和共享限流共同约束执行。
func (s *Service) recoverPendingRotations(ctx context.Context) {
	nodes, err := s.repository.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		s.rotationLog().Warn("egress_rotation_recover_read_failed", "error", err.Error())
		return
	}
	now := time.Now().UTC()
	recovered := 0
	for _, node := range nodes {
		// Webhook 主动轮换与无 Webhook 被动验证(外部代理池/家庭宽带型固定
		// 出口)都经轮换队列恢复,不要求配置 Webhook。
		if !node.Enabled {
			continue
		}
		if node.LastError != domain.LastErrorExitIPQuality || node.CooldownUntil == nil || !now.Before(*node.CooldownUntil) {
			continue
		}
		if err := s.enqueueRotation(node.ID); err != nil {
			s.rotationLog().Warn("egress_rotation_recover_enqueue_failed", "node_id", node.ID, "error", err)
			break
		}
		recovered++
	}
	if recovered > 0 {
		s.rotationLog().Info("egress_rotation_recovered_after_restart", "nodes", recovered)
	}
}

func (r *rotationScheduler) clearLocked() {
	for _, timer := range r.timers {
		timer.Stop()
	}
	clear(r.timers)
	r.queue = nil
	clear(r.set)
	r.epoch++
}
func (r *rotationScheduler) clear() { r.mu.Lock(); r.closed = true; r.clearLocked(); r.mu.Unlock() }
