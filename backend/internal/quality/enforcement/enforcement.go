// Package enforcement observes versioned exit identities and applies the
// quality court's dispositions. Court owns controlled-comparison decisions;
// admission rejection alone does not sentence an exit. A new IP epoch or an
// explicit review can release the corresponding quality restrictions.
//
// Network health, webhook execution and shared rotation capacity remain with
// the network owner. This package submits rotation commands and records facts.
package enforcement

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// ExitIPSource 提供节点当前已知出口身份(被动漂移检测的探测面,
// D6 移植:组合根适配底座探活结果,不重复发起 HTTP 探测)。
type ExitIPSource interface {
	// CurrentExitIdentity returns the per-family exit identity and the
	// database revision issued before measurement. ok=false 表示尚无观测。
	CurrentExitIdentity(ctx context.Context, nodeID uint64) (identity model.ExitIdentity, revision uint64, ok bool, err error)
}

// Rotator submits a command to the network owner, which executes the webhook
// and enforces the shared capacity and per-node attempt budget.
type Rotator interface {
	TriggerRotation(ctx context.Context, nodeID uint64) error
}

// Config 是执行所配置。
type Config struct {
	// PollInterval IP-epoch 检测节拍。
	PollInterval time.Duration
	Logger       *slog.Logger
}

// DefaultConfig sets the identity observation cadence; capacity belongs to the network.
func DefaultConfig() Config {
	return Config{PollInterval: 5 * time.Minute}
}

func (c Config) normalized() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Minute
	}
	return c
}

// ErrRateLimited 轮换限速(排队稍后重试)。
var ErrRateLimited = errors.New("enforcement: 轮换限速")

// Service 是执行所。
type Service struct {
	cfg      Config
	registry *registry.Registry
	ipSource ExitIPSource
	rotator  Rotator
	nodes    proxy.NodeSource
	logger   *slog.Logger
	// lastRotate 每节点上次自动轮换时刻(退避:轮换未致 IP 变化时,
	// 同节点不得每拍重烧全局限速槽——既骚扰上游 webhook,也让
	// 排序在后的被禁节点饿死)。
	lastRotateMu sync.Mutex
	lastRotate   map[uint64]time.Time
	cancel       context.CancelFunc
	done         chan struct{}
}

// New 构建执行所并启动 epoch 检测循环(nodes/ipSource 为 nil 时空转)。
func New(cfg Config, qualityRegistry *registry.Registry, nodes proxy.NodeSource, ipSource ExitIPSource, rotator Rotator) *Service {
	cfg = cfg.normalized()
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	service := &Service{
		cfg: cfg, registry: qualityRegistry, ipSource: ipSource, rotator: rotator,
		nodes: nodes, logger: logger,
		lastRotate: map[uint64]time.Time{},
		done:       make(chan struct{}),
	}
	if service.registry != nil && service.nodes != nil && service.ipSource != nil {
		ctx, cancel := context.WithCancel(context.Background())
		service.cancel = cancel
		go service.run(ctx)
	} else {
		close(service.done)
	}
	return service
}

// Close 停止检测循环(循环未启动时为 no-op——Close 不因空转服务阻塞)。
func (s *Service) Close(ctx context.Context) error {
	if s == nil || s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
		return nil
	default:
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) run(parent context.Context) {
	defer close(s.done)
	// 启动即首询:重建"最近已知 IP"基线,后续轮询检测漂移。
	firstCtx, firstCancel := context.WithTimeout(parent, s.cfg.PollInterval)
	if _, err := s.PollEpochs(firstCtx); err != nil {
		s.logger.Warn("enforcement_epoch_poll_failed", "error", err.Error())
	}
	s.rotateBannedWebhooks(firstCtx)
	firstCancel()
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(parent, s.cfg.PollInterval)
			if _, err := s.PollEpochs(ctx); err != nil {
				s.logger.Warn("enforcement_epoch_poll_failed", "error", err.Error())
			}
			// 轮换后 IP 变化由下一轮检测翻篇——扫掠与检测交替推进自愈。
			s.rotateBannedWebhooks(ctx)
			cancel()
		}
	}
}

// EpochChange 一次检测到的 IP 变化。
type EpochChange struct {
	NodeID   uint64
	OldEpoch uint64
	NewEpoch uint64
	Released bool // 是否自动解除了质量羁押/ban(统一 ban 律)
}

// PollEpochs consumes versioned source observations. The registry atomically
// rejects old versions and releases old restrictions only for a new identity.
func (s *Service) PollEpochs(ctx context.Context) ([]EpochChange, error) {
	profiles, err := s.nodes.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	var changes []EpochChange
	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return changes, err
		}
		if !profile.Enabled {
			continue
		}
		change, changed, err := s.observeNodeExit(ctx, profile)
		if err != nil {
			return changes, err
		}
		if changed {
			changes = append(changes, change)
		}
	}
	return changes, nil
}

// ObserveNodeExit 对单节点立即执行一次出口身份观测(轮换成功后由组合根
// 驱动,不等下一个检测节拍;停用/缺失节点与 PollEpochs 同口径跳过)。
// 观测仍经版本水位校验,迟到或重复调用不会翻篇两次。
func (s *Service) ObserveNodeExit(ctx context.Context, nodeID uint64) (EpochChange, bool, error) {
	if s.registry == nil || s.nodes == nil || s.ipSource == nil {
		return EpochChange{}, false, nil
	}
	profile, ok, err := s.nodes.Profile(ctx, nodeID)
	if err != nil || !ok || !profile.Enabled {
		return EpochChange{}, false, err
	}
	return s.observeNodeExit(ctx, profile)
}

func (s *Service) observeNodeExit(ctx context.Context, profile proxy.NodeProfile) (EpochChange, bool, error) {
	identity, revision, ok, err := s.ipSource.CurrentExitIdentity(ctx, profile.ID)
	if err != nil {
		return EpochChange{}, false, err
	}
	if !ok || !identity.Present() || revision == 0 {
		return EpochChange{}, false, nil
	}
	oldEpoch, newEpoch, released, err := s.registry.ObserveExitIdentity(ctx, profile.ID, identity, revision)
	if err != nil {
		return EpochChange{}, false, err
	}
	if oldEpoch == newEpoch {
		return EpochChange{}, false, nil
	}
	change := EpochChange{
		NodeID: profile.ID, OldEpoch: oldEpoch, NewEpoch: newEpoch, Released: len(released) > 0,
	}
	s.logger.Info("enforcement_epoch_advanced", "node", profile.ID,
		"old_epoch", oldEpoch, "new_epoch", newEpoch, "released", len(released))
	return change, true, nil
}

// rotateBannedWebhooks 自愈扫掠(统一 ban 律的闭环半边):BANNED 的
// webhook 节点自动触发主动轮换——轮换成功后由组合根的观测回调立即
// 翻篇解禁(本包的 ObserveNodeExit),检测节拍兜底被动漂移
// (ban 不再死等人工);固定/池节点不适用(无主动轮换语义)。限速
// 耗尽时安静截止,留待下轮扫掠(节拍天然重试)。
func (s *Service) rotateBannedWebhooks(ctx context.Context) int {
	if s.registry == nil || s.nodes == nil || s.rotator == nil {
		return 0
	}
	rotated := 0
	now := time.Now().UTC()
	for _, key := range s.registry.ListBannedExits() {
		profile, ok, err := s.nodes.Profile(ctx, key.NodeID)
		if err != nil || !ok || profile.Type() != model.NodeWebhook {
			continue
		}
		// 退避窗内的节点跳过(上次轮换尚未生效到 IP 变化的检测窗):
		// 重复触发不会更早解禁,只会烧限速槽+饿死后续节点。
		if s.nodeOnRotateBackoff(key.NodeID, now) {
			continue
		}
		if err := s.rotateAutomatic(ctx, key.NodeID); err != nil {
			if errors.Is(err, ErrRateLimited) {
				return rotated
			}
			s.logger.Warn("enforcement_auto_rotate_failed", "node", key.NodeID, "error", err.Error())
			continue
		}
		s.markRotated(key.NodeID, now)
		rotated++
		s.logger.Info("enforcement_auto_rotated", "node", key.NodeID, "epoch", key.Epoch)
	}
	return rotated
}

// ManualUnban 人工解禁(G7):清除节点当前 epoch 的一切质量处置
// (ban/羁押→可用)。无到期回池(G17)——解禁只有两条路:IP 变化
// 与人工。
func (s *Service) ManualUnban(ctx context.Context, nodeID uint64, reasons ...string) error {
	reason := "operator_exit_release"
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	if err := s.registry.ReleaseCurrentExitAfterReview(ctx, nodeID, reason); err != nil {
		return err
	}
	s.logger.Info("enforcement_manual_unban", "node", nodeID)
	return nil
}

// RecordDegradeEvent 降智台账入口(G8):court 立案时调用;IP 取
// 当前档案(epoch 匹配才记——旧 epoch 事件不追新 IP)。
func (s *Service) RecordDegradeEvent(ctx context.Context, nodeID, epoch uint64, at time.Time) error {
	record, _, err := s.registry.ExitIPAt(ctx, nodeID, epoch)
	if err != nil {
		return err
	}
	ip := record.IP
	return s.registry.AppendDegrade(ctx, nodeID, epoch, ip, at)
}

// AutomaticRotator preserves the executor's durable attempt budget. The
// executor owns global admission for both automatic and manual submissions.
type AutomaticRotator interface {
	TriggerAutomaticRotation(context.Context, uint64) error
}

func (s *Service) rotateAutomatic(ctx context.Context, nodeID uint64) error {
	if rotator, ok := s.rotator.(AutomaticRotator); ok {
		return rotator.TriggerAutomaticRotation(ctx, nodeID)
	}
	return s.RotateNode(ctx, nodeID)
}

// RotateNode 触发单节点 webhook 轮换(G18 的单点入口;webhook 型
// 专用——固定/池节点无轮换语义)。轮换致 IP 变化后由下轮检测
// 自动解禁(G16:IP 变即放,无金丝雀)。
func (s *Service) RotateNode(ctx context.Context, nodeID uint64) error {
	profile, ok, err := s.nodes.Profile(ctx, nodeID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("enforcement: 节点不存在")
	}
	if profile.Type() != model.NodeWebhook {
		return errors.New("enforcement: 仅 webhook 节点支持主动轮换")
	}
	return s.rotator.TriggerRotation(ctx, nodeID)
}

// RotateNodes 批量轮换(G18):多节点一键;限速内逐个触发,
// 超限部分返回 ErrRateLimited 列表。
func (s *Service) RotateNodes(ctx context.Context, nodeIDs []uint64) (triggered, rateLimited int, err error) {
	for _, nodeID := range nodeIDs {
		if rotateErr := s.RotateNode(ctx, nodeID); rotateErr != nil {
			if errors.Is(rotateErr, ErrRateLimited) {
				rateLimited++
				continue
			}
			return triggered, rateLimited, rotateErr
		}
		triggered++
	}
	return triggered, rateLimited, nil
}

// nodeOnRotateBackoff 报告节点是否仍处自动轮换退避窗内
// (窗口=2×检测节拍:轮换后至少经过两轮 IP 检测仍未翻篇才允许
// 再次触发——给异步 webhook 生效时间)。
func (s *Service) nodeOnRotateBackoff(nodeID uint64, now time.Time) bool {
	s.lastRotateMu.Lock()
	defer s.lastRotateMu.Unlock()
	last, ok := s.lastRotate[nodeID]
	return ok && now.Sub(last) < 2*s.cfg.PollInterval
}

// markRotated 记录节点本次自动轮换时刻(仅自动扫掠路径使用;
// 人工 RotateNode 不受退避约束)。
func (s *Service) markRotated(nodeID uint64, at time.Time) {
	s.lastRotateMu.Lock()
	defer s.lastRotateMu.Unlock()
	s.lastRotate[nodeID] = at
}
