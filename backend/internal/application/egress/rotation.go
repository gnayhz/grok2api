package egress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// DefaultRotationConfig returns conservative defaults.
func DefaultRotationConfig() RotationConfig {
	return RotationConfig{
		Enabled:                  true,
		MaxAttemptsPerQuarantine: 3,
		MinNodeInterval:          3 * time.Minute,
		MaxGlobalPerHour:         6,
		WebhookTimeout:           15 * time.Second,
		WebhookRetries:           2,
		SettleDelay:              20 * time.Second,
		ProbeTimeout:             2 * time.Minute,
		ProbeInterval:            5 * time.Second,
	}
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
	mu    sync.Mutex
	set   map[uint64]struct{}
	queue []uint64
	wake  chan struct{}

	// epoch 在每次启用状态变更时递增;requeueAfter 的挂起定时器携带其
	// 创建时的 epoch,到点后仅当 epoch 未变才回流——"禁用丢弃全部排队
	// 工作"的契约否则会被未到期的定时器绕过(限速/最小间隔的重排到点后
	// 把节点送回已禁用的队列)。
	epoch uint64

	hourStart time.Time
	hourCount int
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
		if !cfg.Enabled {
			return
		}
		s.rotation = &rotationScheduler{set: make(map[uint64]struct{}), wake: make(chan struct{}, 1)}
	} else if !cfg.Enabled {
		s.rotation.mu.Lock()
		s.rotation.queue = nil
		s.rotation.set = make(map[uint64]struct{})
		s.rotation.epoch++
		s.rotation.mu.Unlock()
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

// SetEgressQualityProber installs the canary verifier implemented by the
// gateway.
func (s *Service) SetEgressQualityProber(value EgressQualityProber) {
	if s == nil || value == nil {
		return
	}
	s.mu.Lock()
	s.qualityProber = value
	s.mu.Unlock()
}

// RotateNode enqueues one node for immediate rotation (manual trigger).
func (s *Service) RotateNode(ctx context.Context, nodeID uint64) error {
	if s == nil || s.repository == nil {
		return ErrOperationsUnavailable
	}
	if _, err := s.repository.GetEgressNode(ctx, nodeID); err != nil {
		// 与其他节点路由一致:缺失节点归一为应用层 ErrNotFound(404),
		// 而不是把 repository 错误透传成 500。
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.mu.RLock()
	enabled := s.rotationCfg.Enabled
	s.mu.RUnlock()
	if !enabled {
		return errors.New("出口轮换未启用")
	}
	s.enqueueRotation(nodeID)
	return nil
}

func (s *Service) enqueueRotation(nodeID uint64) {
	if s == nil || nodeID == 0 {
		return
	}
	s.mu.RLock()
	rotation := s.rotation
	enabled := s.rotationCfg.Enabled
	s.mu.RUnlock()
	if rotation == nil || !enabled {
		return
	}
	rotation.mu.Lock()
	if _, queued := rotation.set[nodeID]; !queued {
		rotation.set[nodeID] = struct{}{}
		rotation.queue = append(rotation.queue, nodeID)
	}
	rotation.mu.Unlock()
	select {
	case rotation.wake <- struct{}{}:
	default:
	}
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
	if _, queued := r.set[nodeID]; !queued {
		r.set[nodeID] = struct{}{}
		r.queue = append(r.queue, nodeID)
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
	epoch := r.epoch
	r.mu.Unlock()
	time.AfterFunc(delay, func() {
		r.mu.Lock()
		if r.epoch != epoch {
			r.mu.Unlock()
			return
		}
		if _, queued := r.set[nodeID]; !queued {
			r.set[nodeID] = struct{}{}
			r.queue = append(r.queue, nodeID)
		}
		r.mu.Unlock()
		select {
		case r.wake <- struct{}{}:
		default:
		}
	})
}

// allowGlobal returns the delay before another rotation may run (0 = now).
func (r *rotationScheduler) allowGlobal(maxPerHour int) time.Duration {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hourStart.IsZero() || now.Sub(r.hourStart) >= time.Hour {
		r.hourStart = now
		r.hourCount = 0
	}
	if r.hourCount < maxPerHour {
		r.hourCount++
		return 0
	}
	return r.hourStart.Add(time.Hour).Sub(now)
}

// RunRotationWorker drains the rotation queue until ctx ends. Exactly one
// worker per process; multi-instance deployments rely on node-state
// bookkeeping (attempts, last rotated) so redundant work converges safely.
// 同步运行消费循环(由调用方 goroutine 托管, Run 的 WaitGroup 因此能等待真实
// worker 退出, 关闭顺序不再与 DB 关闭竞争); processRotation 经 batch.Do 隔离
// panic——轮换链路(webhook/解密/探测/canary 推理)任一 panic 不得击穿进程。
func (s *Service) RunRotationWorker(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.RLock()
	rotation := s.rotation
	enabled := s.rotationCfg.Enabled
	s.mu.RUnlock()
	if rotation == nil || !enabled {
		return
	}
	s.recoverPendingRotations(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-rotation.wake:
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
	logger := s.rotationLog()
	s.mu.RLock()
	cfg := s.rotationCfg
	qualityCfg := s.qualityGuard.normalized()
	quarantiner := s.qualityQuarantiner
	prober := s.qualityProber
	cipher := s.cipher
	s.mu.RUnlock()
	if quarantiner == nil {
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
		s.recordRotationState(ctx, nodeID, node.RotationAttempts, "no rotation webhook configured", false)
		logger.Info("egress_rotation_skipped", "node_id", nodeID, "node", node.Name, "reason", "no webhook")
		return
	}
	if !node.RotationEnabled {
		s.recordRotationState(ctx, nodeID, node.RotationAttempts, "rotation disabled for this node", false)
		logger.Info("egress_rotation_skipped", "node_id", nodeID, "node", node.Name, "reason", "rotation disabled")
		return
	}
	rotationURL, err := cipher.Decrypt(node.EncryptedRotationURL)
	if err != nil {
		s.recordRotationState(ctx, nodeID, node.RotationAttempts, "decrypt rotation url: "+err.Error(), false)
		logger.Warn("egress_rotation_decrypt_failed", "node_id", nodeID, "error", err.Error())
		return
	}
	if node.RotationAttempts >= cfg.MaxAttemptsPerQuarantine {
		logger.Warn("egress_rotation_exhausted", "node_id", nodeID, "node", node.Name, "attempts", node.RotationAttempts, "max", cfg.MaxAttemptsPerQuarantine)
		perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "exhausted"})
		s.recordRotationState(ctx, nodeID, node.RotationAttempts, "rotation attempts exhausted", false)
		return
	}
	// 未到 MinNodeInterval:重排队尾让 worker 立即处理下一个到期节点, 而不是
	// 原地睡眠阻塞整个队列(此前一次等待最长 10 分钟, 其间其他节点的隔离
	// 轮换全部停滞)。
	if node.LastRotatedAt != nil {
		if wait := cfg.MinNodeInterval - time.Since(*node.LastRotatedAt); wait > 0 {
			s.recordRotationState(ctx, nodeID, node.RotationAttempts, "min interval not elapsed", false)
			if s.rotation != nil {
				s.rotation.requeueAfter(nodeID, wait)
			}
			return
		}
	}
	if s.rotation != nil {
		if wait := s.rotation.allowGlobal(cfg.MaxGlobalPerHour); wait > 0 {
			logger.Info("egress_rotation_rate_limited", "node_id", nodeID, "retry_in", wait.Round(time.Second).String())
			s.rotation.requeueAfter(nodeID, wait)
			return
		}
	}
	if err := s.callRotationWebhook(ctx, rotationURL, cfg); err != nil {
		s.recordRotationState(ctx, nodeID, node.RotationAttempts, "webhook: "+err.Error(), false)
		logger.Warn("egress_rotation_webhook_failed", "node_id", nodeID, "error", err.Error())
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
	// 死出口轮换(LastError=transport 的探活确认触发): 隧道已重启且探活
	// 健康, 目的即达成——健康探活已按 last_error=transport 自动清除冷却
	// (repository CASE 分支), 节点已回池。不走 canary(质量判决与"隧道
	// 复活"正交), 也无需解除质量隔离(本就没有质量隔离)。
	if node.LastError == domain.LastErrorTransport {
		s.recordRotationState(ctx, nodeID, 0, "", true)
		logger.Info("egress_rotation_succeeded", "node_id", nodeID, "node", node.Name, "exit_ip", probe.ExitIP, "exit_ip_v6", probe.IPv6.ExitIP, "reason", "probe_dead_recovered")
		perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "succeeded"})
		return
	}
	now := time.Now().UTC()
	if prober == nil {
		// No canary wiring: tentative re-admission with a short cooldown. The
		// passive guard re-quarantines cheaply if the IP is still degraded.
		if err := quarantiner.CooldownNodeForQuality(ctx, nodeID, now.Add(qualityCfg.TentativeReleaseCooldown)); err != nil {
			logger.Warn("egress_rotation_tentative_failed", "node_id", nodeID, "error", err.Error())
			return
		}
		s.recordRotationState(ctx, nodeID, 0, "", true)
		logger.Warn("egress_rotation_tentative_release", "node_id", nodeID, "node", node.Name, "exit_ip", probe.ExitIP, "exit_ip_v6", probe.IPv6.ExitIP, "cooldown", qualityCfg.TentativeReleaseCooldown.String())
		perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "tentative_release"})
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	result := prober.ProbeEgressQuality(probeCtx, nodeID)
	cancel()
	switch result.Outcome {
	case EgressQualityProbeClean:
		if err := quarantiner.ReleaseQualityQuarantine(ctx, nodeID); err != nil {
			logger.Warn("egress_rotation_release_failed", "node_id", nodeID, "error", err.Error())
			return
		}
		s.recordRotationState(ctx, nodeID, 0, "", true)
		logger.Info("egress_rotation_succeeded", "node_id", nodeID, "node", node.Name, "exit_ip", probe.ExitIP, "exit_ip_v6", probe.IPv6.ExitIP)
		perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "succeeded"})
	case EgressQualityProbeDegraded:
		s.failRotation(ctx, nodeID, &node, cfg, "canary degraded: "+result.Reason, logger)
	case EgressQualityProbeNoAccount, EgressQualityProbeUnconfigured:
		// 未配置 canary 模型(EXIT-IP-GUARD 文档承诺的默认形态)与无可用账号
		// 同样处理:换 IP 已成功且出口 IP 已变化,以短冷却暂定放行,被动守卫
		// 继续兜底——而不是把节点扣满整个隔离周期。
		if err := quarantiner.CooldownNodeForQuality(ctx, nodeID, now.Add(qualityCfg.TentativeReleaseCooldown)); err != nil {
			logger.Warn("egress_rotation_tentative_failed", "node_id", nodeID, "error", err.Error())
			return
		}
		s.recordRotationState(ctx, nodeID, 0, "", true)
		reason := "canary account unavailable"
		if result.Outcome == EgressQualityProbeUnconfigured {
			reason = "canary model not configured"
		}
		logger.Warn("egress_rotation_tentative_release", "node_id", nodeID, "reason", reason, "cooldown", qualityCfg.TentativeReleaseCooldown.String())
		perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "tentative_release"})
	default:
		// Errored canary: keep the node quarantined and stop; a fresh
		// quarantine event re-enqueues rotation later.
		s.recordRotationState(ctx, nodeID, node.RotationAttempts, "canary "+string(result.Outcome)+": "+result.Reason, true)
		logger.Warn("egress_rotation_canary_inconclusive", "node_id", nodeID, "outcome", string(result.Outcome), "reason", result.Reason)
	}
}

// failRotation records one failed rotation attempt and re-enqueues when the
// attempt budget allows another try.
func (s *Service) failRotation(ctx context.Context, nodeID uint64, node *domain.Node, cfg RotationConfig, reason string, logger *slog.Logger) {
	perfmetrics.Default.Inc("egress_rotation_total", perfmetrics.Labels{Subsystem: "egress", Operation: "rotation", Outcome: "failed"})
	attempts := node.RotationAttempts + 1
	s.recordRotationState(ctx, nodeID, attempts, reason, true)
	logger.Warn("egress_rotation_failed", "node_id", nodeID, "attempt", attempts, "max", cfg.MaxAttemptsPerQuarantine, "reason", reason)
	if attempts < cfg.MaxAttemptsPerQuarantine {
		s.enqueueRotation(nodeID)
	}
}

// recordRotationState persists one rotation attempt's bookkeeping. rotated
// controls whether LastRotatedAt advances: skip paths (no webhook, disabled,
// exhausted, decrypt or webhook failure) must not extend MinNodeInterval --
// the node never actually rotated.
func (s *Service) recordRotationState(ctx context.Context, nodeID uint64, attempts int, lastError string, rotated bool) {
	if stateRepo, ok := s.repository.(rotationStateRepository); ok {
		now := time.Now().UTC()
		var rotatedAt *time.Time
		if rotated {
			rotatedAt = &now
		}
		if err := stateRepo.UpdateEgressNodeRotationState(ctx, nodeID, rotatedAt, attempts, truncString(lastError, 512)); err != nil {
			s.rotationLog().Warn("egress_rotation_state_failed", "node_id", nodeID, "error", err.Error())
		}
	}
}

type rotationStateRepository interface {
	UpdateEgressNodeRotationState(ctx context.Context, id uint64, lastRotatedAt *time.Time, attempts int, lastError string) error
}

func (s *Service) callRotationWebhook(ctx context.Context, rotationURL string, cfg RotationConfig) error {
	client := &http.Client{Timeout: cfg.WebhookTimeout}
	var lastErr error
	for attempt := 0; attempt <= cfg.WebhookRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, rotationURL, bytes.NewReader([]byte("{}")))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("webhook HTTP %d", response.StatusCode)
	}
	return lastErr
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
	deadline := time.Now().Add(cfg.ProbeTimeout)
	var last domain.ProbeResult
	for {
		result, err := s.testNode(ctx, nodeID, false)
		if err != nil {
			return result, err
		}
		if result.Status == domain.ProbeStatusHealthy {
			return result, nil
		}
		last = result
		if time.Now().After(deadline) {
			return last, nil
		}
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
// 尝试计数/LastRotatedAt/限流守卫与多实例语义(节点行账本收敛)不变。
func (s *Service) recoverPendingRotations(ctx context.Context) {
	nodes, err := s.repository.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		s.rotationLog().Warn("egress_rotation_recover_read_failed", "error", err.Error())
		return
	}
	now := time.Now().UTC()
	recovered := 0
	for _, node := range nodes {
		if !node.Enabled || !node.RotationEnabled || node.EncryptedRotationURL == "" {
			continue
		}
		if node.LastError != domain.LastErrorExitIPQuality || node.CooldownUntil == nil || !now.Before(*node.CooldownUntil) {
			continue
		}
		s.enqueueRotation(node.ID)
		recovered++
	}
	if recovered > 0 {
		s.rotationLog().Info("egress_rotation_recovered_after_restart", "nodes", recovered)
	}
}
