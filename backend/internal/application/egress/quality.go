package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// QualityGuardConfig controls exit-IP quality quarantine. All durations and
// thresholds are runtime-updatable; zero values keep the defaults below.
type QualityGuardConfig struct {
	// QuarantineCooldown keeps a degraded node out of rotation while its exit
	// IP is rotated and verified. A successful verification releases early.
	QuarantineCooldown time.Duration
	// CrossAccountThreshold is the distinct-account count inside
	// CrossAccountWindow that quarantines a node without RSC attribution
	// (attribution disabled, unlinked account, or attribution still pending).
	// Values below 2 disable the fallback.
	CrossAccountThreshold int
	// CrossAccountWindow bounds how long degrade evidence stays decisive.
	CrossAccountWindow time.Duration
	// TentativeReleaseCooldown applies when rotation finished but verification
	// was inconclusive (no canary model/account): the node returns to rotation
	// but keeps a short cooldown so the passive guard re-quarantines cheaply.
	TentativeReleaseCooldown time.Duration
}

// DefaultQualityGuardConfig returns conservative defaults: 24h quarantine,
// two distinct accounts within 30 minutes confirm an exit-IP degrade.
func DefaultQualityGuardConfig() QualityGuardConfig {
	return QualityGuardConfig{
		QuarantineCooldown:       24 * time.Hour,
		CrossAccountThreshold:    2,
		CrossAccountWindow:       30 * time.Minute,
		TentativeReleaseCooldown: 30 * time.Minute,
	}
}

func (c QualityGuardConfig) normalized() QualityGuardConfig {
	if c.QuarantineCooldown <= 0 {
		c.QuarantineCooldown = 24 * time.Hour
	}
	if c.CrossAccountWindow <= 0 {
		c.CrossAccountWindow = 30 * time.Minute
	}
	if c.TentativeReleaseCooldown <= 0 {
		c.TentativeReleaseCooldown = 30 * time.Minute
	}
	return c
}

// QualityQuarantiner is the infra-level quarantine surface implemented by the
// egress Manager.
type QualityQuarantiner interface {
	QuarantineNodeForQuality(ctx context.Context, nodeID uint64, until time.Time) (domain.Node, error)
	MarkDegradeEvidence(nodeID uint64)
	ClearDegradeEvidence(nodeID uint64)
	ReleaseQualityQuarantine(ctx context.Context, nodeID uint64) error
	// CooldownNodeForQuality applies a plain cooldown without touching
	// degrade counters; used for tentative re-admission after inconclusive
	// verification.
	CooldownNodeForQuality(ctx context.Context, nodeID uint64, until time.Time) error
	// CooldownNodeForProbeFailure applies a transport cooldown after a
	// confirmed dead exit (both address families failed twice in a row).
	// Recovery is automatic: the next healthy probe clears it.
	CooldownNodeForProbeFailure(ctx context.Context, nodeID uint64, until time.Time) error
}

// EgressQualityProber verifies one node's exit IP with a minimal real
// inference request (the canary). Implemented by the gateway. An
// unconfigured/error result leaves verification inconclusive.
type EgressQualityProber interface {
	ProbeEgressQuality(ctx context.Context, nodeID uint64) EgressQualityProbeResult
}

// EgressQualityProbeOutcome classifies one canary verification.
type EgressQualityProbeOutcome string

const (
	EgressQualityProbeClean        EgressQualityProbeOutcome = "clean"
	EgressQualityProbeDegraded     EgressQualityProbeOutcome = "degraded"
	EgressQualityProbeNoAccount    EgressQualityProbeOutcome = "no_account"
	EgressQualityProbeUnconfigured EgressQualityProbeOutcome = "unconfigured"
	EgressQualityProbeError        EgressQualityProbeOutcome = "error"
)

// EgressQualityProbeResult is the canary outcome plus a short reason for logs.
type EgressQualityProbeResult struct {
	Outcome EgressQualityProbeOutcome
	Reason  string
}

type degradeObservation struct {
	accountID uint64
	at        time.Time
}

// SetQualityQuarantiner installs the infra quarantine adapter.
func (s *Service) SetQualityQuarantiner(value QualityQuarantiner) {
	if s == nil || value == nil {
		return
	}
	s.mu.Lock()
	s.qualityQuarantiner = value
	s.mu.Unlock()
}

// SetQualityGuardConfig updates guard thresholds at runtime.
func (s *Service) SetQualityGuardConfig(value QualityGuardConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.qualityGuard = value.normalized()
	s.mu.Unlock()
}

// SetQualityLogger installs a dedicated logger for the quality guard.
// MarkDegradeEvidence applies the pending soft cooldown on the serving node
// after one degrade verdict (withhold / idle abort). Implements the gateway
// observer surface; never blocks the request path.
func (s *Service) MarkDegradeEvidence(nodeID uint64) {
	if s == nil || nodeID == 0 {
		return
	}
	s.mu.RLock()
	quarantiner := s.qualityQuarantiner
	s.mu.RUnlock()
	if quarantiner == nil {
		return
	}
	quarantiner.MarkDegradeEvidence(nodeID)
}

// ClearDegradeEvidence lifts the pending soft cooldown when attribution
// exonerates the node (RSC RISK).
func (s *Service) ClearDegradeEvidence(nodeID uint64) {
	if s == nil || nodeID == 0 {
		return
	}
	s.mu.RLock()
	quarantiner := s.qualityQuarantiner
	s.mu.RUnlock()
	if quarantiner == nil {
		return
	}
	quarantiner.ClearDegradeEvidence(nodeID)
}

// RemoveAccountEvidence 从跨账号确认窗口移除一个账号的全部观测(RSC 判账号
// 有罪时调用):残留观测会在窗口期内与另一账号的一次瞬时降智凑满阈值, 把
// 无辜节点错杀进 24h 隔离并真实触发远端换 IP。窗口因此清空的节点一并删除键。
func (s *Service) RemoveAccountEvidence(nodeID, accountID uint64) {
	if s == nil || nodeID == 0 {
		return
	}
	s.qualityMu.Lock()
	observations := s.qualityEvidence[nodeID]
	kept := make([]degradeObservation, 0, len(observations))
	for _, observation := range observations {
		if observation.accountID == accountID {
			continue
		}
		kept = append(kept, observation)
	}
	if len(kept) == 0 {
		delete(s.qualityEvidence, nodeID)
	} else {
		s.qualityEvidence[nodeID] = kept
	}
	s.qualityMu.Unlock()
}

// ReleaseIfEvidenceOnlyFrom:节点处于出口质量隔离(LastError==exit_ip_quality
// 且冷却未到期)时, 若当前窗口内的证据确实存在且全部来自 guilty 账号, 则解除
// 隔离——归因已证明是账号问题, 隔离依据不复存在。证据含其他账号时保留隔离,
// 交被动守卫继续观察;窗口为空(确认时删除/自然过期)同样保留——空洞真值不算
// "全部来自有罪账号"。调用方(risk 服务)须在 RemoveAccountEvidence 之前调用,
// 否则有罪观测已消失,"仅含有罪账号"不可判定。实现 risk.EgressEvidenceRemover。
func (s *Service) ReleaseIfEvidenceOnlyFrom(ctx context.Context, nodeID, guiltyAccountID uint64) {
	if s == nil || nodeID == 0 {
		return
	}
	s.mu.RLock()
	quarantiner := s.qualityQuarantiner
	s.mu.RUnlock()
	if quarantiner == nil || s.repository == nil {
		return
	}
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if err != nil || node.LastError != domain.LastErrorExitIPQuality {
		return
	}
	if node.CooldownUntil == nil || !time.Now().UTC().Before(*node.CooldownUntil) {
		return
	}
	s.qualityMu.Lock()
	others := false
	fromGuilty := false
	for _, observation := range s.qualityEvidence[nodeID] {
		if observation.accountID == guiltyAccountID {
			fromGuilty = true
			continue
		}
		others = true
		break
	}
	s.qualityMu.Unlock()
	// 空窗口不释放:跨账号确认在确认时删除证据,30m 窗口也会在 24h 隔离内
	// 自然过期——空集上"证据全部来自有罪账号"是空洞真值。单个 RISK 判决只
	// 否定该账号自己的降智贡献,未否定确认依据;隔离由冷却到期/轮换收尾。
	// 释放要求窗口确实把证据归到有罪账号名下(调用方须在移除观测前检查)。
	if others || !fromGuilty {
		return
	}
	if releaser, ok := quarantiner.(interface {
		ReleaseQualityQuarantine(ctx context.Context, nodeID uint64) error
	}); ok {
		if err := releaser.ReleaseQualityQuarantine(ctx, nodeID); err != nil {
			s.qualityLog().Warn("egress_risk_release_failed", "node_id", nodeID, "error", err.Error())
			return
		}
		s.qualityLog().Info("egress_risk_released", "node_id", nodeID, "guilty_account_id", guiltyAccountID)
	}
}

func (s *Service) SetQualityLogger(value *slog.Logger) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if value != nil {
		s.qualityLogger = value
	}
	s.mu.Unlock()
}

func (s *Service) qualityLog() *slog.Logger {
	if s == nil {
		return slog.Default()
	}
	s.mu.RLock()
	logger := s.qualityLogger
	s.mu.RUnlock()
	if logger == nil {
		return slog.Default()
	}
	return logger
}

// OnEgressDegraded records exit-IP degrade evidence from the real-time guard.
// It implements gateway.EgressDegradationObserver. When RSC attribution is
// unavailable (disabled, unlinked, or still running), distinct accounts
// degrading on the same node inside the window confirm an exit-IP problem and
// quarantine the node. Recording is cheap; quarantine runs detached.
func (s *Service) OnEgressDegraded(ctx context.Context, nodeID, accountID uint64) {
	if s == nil || s.repository == nil || nodeID == 0 {
		return
	}
	s.mu.RLock()
	cfg := s.qualityGuard
	quarantiner := s.qualityQuarantiner
	s.mu.RUnlock()
	cfg = cfg.normalized()
	if quarantiner == nil || cfg.CrossAccountThreshold < 2 {
		return
	}
	now := time.Now().UTC()
	s.qualityMu.Lock()
	observations := append(s.qualityEvidence[nodeID], degradeObservation{accountID: accountID, at: now})
	kept := make([]degradeObservation, 0, len(observations))
	distinct := make(map[uint64]struct{}, len(observations))
	for _, observation := range observations {
		if now.Sub(observation.at) > cfg.CrossAccountWindow {
			continue
		}
		kept = append(kept, observation)
		distinct[observation.accountID] = struct{}{}
	}
	s.qualityEvidence[nodeID] = kept
	confirmed := len(distinct) >= cfg.CrossAccountThreshold
	if confirmed {
		delete(s.qualityEvidence, nodeID)
	}
	s.qualityMu.Unlock()
	if !confirmed {
		return
	}
	perfmetrics.Default.Inc("egress_quality_degrade_total", perfmetrics.Labels{Subsystem: "egress", Operation: "cross_account_confirm", Outcome: "confirmed"})
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	go func() {
		defer cancel()
		// 隔离链路(读节点/写库/入队)panic 不得击穿进程:batch.Do 转 PanicError 记日志。
		if err := batch.Do(detached, func(taskCtx context.Context) error {
			s.QuarantineForExitIP(taskCtx, nodeID, accountID)
			return nil
		}); err != nil {
			var panicErr *batch.PanicError
			if errors.As(err, &panicErr) {
				s.qualityLog().Error("egress_quarantine_panic", "node_id", nodeID, "panic", panicErr.Value, "stack", string(panicErr.Stack))
			}
		}
	}()
}

// OnRscCleanDegrade 是 RSC clean 判决(账号被还清白,出口 IP 成为唯一嫌疑)的
// 隔离入口,实现 risk.EgressQuarantiner。单次头迟滞等实时信号偶发误伤健康
// 节点(冷连接慢头/上游瞬时负载),一次排除法不足以定罪 24h:要求
// crossAccountWindow 窗口内已有第二次降智观测(不同账号,或同账号再犯——
// 每次都经 RSC 洗清账号自身)才升级硬隔离。首次只保留 L2 软冷却(由
// MarkDegradeEvidence 在信号侧即时写入)并记 pending,不隔离。跨账号确认
// 机制被关闭(threshold<2,窗口不再记录观测)时退回旧行为:立即隔离。
// 跨账号确认路径(OnEgressDegraded 已集齐两个不同账号)不经此门槛。
func (s *Service) OnRscCleanDegrade(ctx context.Context, nodeID, degradedAccountID uint64) {
	if s == nil || s.repository == nil || nodeID == 0 {
		return
	}
	s.mu.RLock()
	cfg := s.qualityGuard
	s.mu.RUnlock()
	cfg = cfg.normalized()
	if cfg.CrossAccountThreshold < 2 {
		// 确认机器关闭:RSC clean 仍是一次权威排除,保持立即隔离。
		s.QuarantineForExitIP(ctx, nodeID, degradedAccountID)
		return
	}
	now := time.Now().UTC()
	s.qualityMu.Lock()
	count := 0
	kept := make([]degradeObservation, 0, len(s.qualityEvidence[nodeID]))
	for _, observation := range s.qualityEvidence[nodeID] {
		if now.Sub(observation.at) > cfg.CrossAccountWindow {
			continue
		}
		kept = append(kept, observation)
		count++
	}
	s.qualityEvidence[nodeID] = kept
	s.qualityMu.Unlock()
	if count < 2 {
		perfmetrics.Default.Inc("egress_quality_quarantine_total", perfmetrics.Labels{Subsystem: "egress", Operation: "quarantine", Outcome: "pending_confirmation"})
		s.qualityLog().Info("egress_quarantine_pending_confirmation", "node_id", nodeID, "account_id", degradedAccountID, "observations", count, "required", 2, "window", cfg.CrossAccountWindow.String())
		return
	}
	s.QuarantineForExitIP(ctx, nodeID, degradedAccountID)
}

// QuarantineForExitIP isolates a node whose exit IP is quality-degraded,
// migrates its auto-bound accounts to healthy nodes, and enqueues an exit-IP
// rotation. Idempotent: an already-quarantined node only re-checks the rotation
// queue.
func (s *Service) QuarantineForExitIP(ctx context.Context, nodeID, degradedAccountID uint64) {
	if s == nil || s.repository == nil || nodeID == 0 {
		return
	}
	logger := s.qualityLog()
	s.mu.RLock()
	cfg := s.qualityGuard
	quarantiner := s.qualityQuarantiner
	s.mu.RUnlock()
	cfg = cfg.normalized()
	if quarantiner == nil {
		return
	}
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		logger.Warn("egress_quality_quarantine_read_failed", "node_id", nodeID, "error", err.Error())
		return
	}
	now := time.Now().UTC()
	alreadyQuarantined := node.CooldownUntil != nil && node.CooldownUntil.After(now) && node.LastError == domain.LastErrorExitIPQuality
	if !alreadyQuarantined {
		if _, err := quarantiner.QuarantineNodeForQuality(ctx, nodeID, now.Add(cfg.QuarantineCooldown)); err != nil {
			logger.Warn("egress_quality_quarantine_failed", "node_id", nodeID, "error", err.Error())
			return
		}
		perfmetrics.Default.Inc("egress_quality_quarantine_total", perfmetrics.Labels{Subsystem: "egress", Operation: "quarantine", Outcome: "applied"})
		logger.Warn("egress_node_quarantined", "node_id", nodeID, "node", node.Name, "degraded_account_id", degradedAccountID, "cooldown", cfg.QuarantineCooldown.String())
	}
	// Rotation is enqueued for both fresh and repeat quarantines: the scheduler
	// deduplicates and honors per-node intervals.
	s.enqueueRotation(nodeID)
}

// BatchRotationResult reports a batch template application.
type BatchRotationResult struct {
	Updated int
	Skipped int
}

// BatchSetNodeRotation applies one rotation-webhook template to many nodes at
// once. Supported placeholders: {name} (node name, URL-escaped), {host} and
// {port} (from the node's decrypted proxy URL). An empty template clears the
// webhook for the selected nodes. Nodes whose proxy URL cannot be resolved
// (e.g. no port while the template references {port}) are skipped, not failed.
func (s *Service) BatchSetNodeRotation(ctx context.Context, ids []uint64, template string) (BatchRotationResult, error) {
	result := BatchRotationResult{}
	if s == nil || s.repository == nil {
		return result, ErrOperationsUnavailable
	}
	if len(ids) == 0 || len(ids) > 5000 {
		return result, fmt.Errorf("%w: 节点数量必须在 1 到 5000 之间", ErrInvalidInput)
	}
	template = strings.TrimSpace(template)
	if template != "" {
		// 占位符先替换成样本再校验：{host}/{name} 中的花括号会被 url.Parse 拒绝。
		sample := strings.NewReplacer("{name}", "sample", "{host}", "example.invalid", "{port}", "1").Replace(template)
		value, err := url.Parse(sample)
		if err != nil || (value.Scheme != "http" && value.Scheme != "https") || value.Host == "" {
			return result, fmt.Errorf("%w: 换 IP webhook 模板必须是 http(s) URL", ErrInvalidInput)
		}
		if len(template) > maxRotationURLBytes {
			return result, fmt.Errorf("%w: 换 IP webhook 模板过长", ErrInvalidInput)
		}
	}
	repo, ok := s.repository.(rotationURLWriter)
	if !ok {
		return result, ErrOperationsUnavailable
	}
	for _, id := range ids {
		node, err := s.repository.GetEgressNode(ctx, id)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				result.Skipped++
				continue
			}
			return result, err
		}
		if template == "" {
			if err := repo.UpdateEgressNodeRotationURL(ctx, id, "", false); err != nil {
				return result, err
			}
			result.Updated++
			continue
		}
		resolved, resolvable := resolveRotationTemplate(template, node, s.cipher)
		if !resolvable {
			result.Skipped++
			continue
		}
		encrypted, err := s.cipher.Encrypt(resolved)
		if err != nil {
			return result, err
		}
		if err := repo.UpdateEgressNodeRotationURL(ctx, id, encrypted, true); err != nil {
			return result, err
		}
		result.Updated++
	}
	return result, nil
}

type rotationURLWriter interface {
	// UpdateEgressNodeRotationURL 同步落库 webhook 密文与启用开关:写入非空
	// webhook 即启用轮换, 清空即关闭——与单节点编辑语义、以及 schema 一次性
	// 回填(enabled 跟随 encrypted_rotation_url)的语义保持一致, 避免按部署
	// 指南批量设置后自动换 IP 闭环静默失效。
	UpdateEgressNodeRotationURL(ctx context.Context, id uint64, encryptedRotationURL string, enabled bool) error
}

// resolveRotationTemplate substitutes {name}/{host}/{port} for one node. The
// second return is false when the template needs a port the proxy URL lacks.
func resolveRotationTemplate(template string, node domain.Node, cipher security.Cryptor) (string, bool) {
	proxyURL := ""
	if cipher != nil && strings.TrimSpace(node.EncryptedProxyURL) != "" {
		if decrypted, err := cipher.Decrypt(node.EncryptedProxyURL); err == nil {
			proxyURL = decrypted
		}
	}
	var host, port string
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil && parsed.Host != "" {
			host, port = "", ""
			if splitHost, splitPort, splitErr := net.SplitHostPort(parsed.Host); splitErr == nil {
				host, port = splitHost, splitPort
			} else {
				host = parsed.Hostname()
			}
			if port == "" {
				port = parsed.Port()
			}
		}
	}
	if strings.Contains(template, "{port}") && port == "" {
		return "", false
	}
	resolved := strings.ReplaceAll(template, "{name}", url.PathEscape(node.Name))
	resolved = strings.ReplaceAll(resolved, "{host}", host)
	resolved = strings.ReplaceAll(resolved, "{port}", port)
	return resolved, true
}
