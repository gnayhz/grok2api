package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var ErrSubscriptionSync = errors.New("代理订阅同步失败")

// hygieneSaveAttempts 限制路由卫生检查条件写的重读重算重试次数:冲突通常
// 是单次并发管理员提交,3 次足以跨过;持续冲突则把错误交还调用方(同步
// 路径按尽力而为丢弃并等待下一次同步自愈)。
const hygieneSaveAttempts = 3

func (s *Service) syncSource(ctx context.Context, operations OperationsRepository, source domain.SubscriptionSource) (ImportResult, error) {
	now := time.Now().UTC()
	nextSyncAt := sourceNextSyncAt(source, now)
	recordFailure := func() {
		// The source URL and any transport detail are deliberately omitted from
		// persisted status and API errors; they may contain subscription tokens.
		_ = operations.UpdateEgressSourceSync(context.WithoutCancel(ctx), source.ID, now, nextSyncAt, 0, "订阅拉取或解析失败")
	}
	if strings.TrimSpace(source.EncryptedURL) == "" {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	urlValue, err := s.cipher.Decrypt(source.EncryptedURL)
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	fetchProxy, err := s.subscriptionFetchProxy(source)
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	content, err := fetchProxySubscription(ctx, urlValue, fetchProxy)
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	entries, skipped, err := parseProxySubscription(string(content))
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	nodes := make([]domain.Node, 0, len(entries))
	for index, entry := range entries {
		encryptedProxy, encryptErr := s.cipher.Encrypt(entry.ProxyURL)
		if encryptErr != nil {
			recordFailure()
			return ImportResult{}, fmt.Errorf("%w: 加密导入节点", ErrSubscriptionSync)
		}
		nodes = append(nodes, domain.Node{
			Name: sourceNodeName(source.Name, index), Enabled: true,
			SourceID: source.ID, SourceKey: entry.Key,
			EncryptedProxyURL: encryptedProxy, Health: 1, ProbeStatus: domain.ProbeStatusUnknown,
		})
	}
	imported, err := operations.UpsertEgressNodesFromSource(ctx, source.ID, nodes)
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	s.invalidateOperationsConfig()
	// Subscription upserts bypass the node-edit guard: a refreshed entry can
	// turn a fixed routing target into an account-bound template. Best effort -
	// a hygiene failure must not fail the sync that already committed.
	_ = s.enforceRoutingHygieneAfterSync(ctx, operations)
	if err := operations.UpdateEgressSourceSync(ctx, source.ID, now, nextSyncAt, imported, ""); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Imported: imported, Skipped: skipped}, nil
}

// enforceRoutingHygieneAfterSync strips routing targets whose node can no
// longer serve them after a subscription sync. The repository hygiene inside
// the sync transaction cannot decrypt proxy URLs, so an entry that became an
// {account} template would otherwise keep routing a "fixed" exit that
// actually rotates per caller identity.
//
// 写回契约:后台写者不得覆盖并发管理员提交。旧实现每次同步都无条件整行
// 写回——即使没有任何目标需要剥离,也会把 [读取快照, 写回] 窗口内落库的
// 管理员修改(检测间隔、探测提供方、路由目标)静默回滚。现在:没有目标被
// 剥离时不写;需要写时优先走条件写(快照以来未变才落库),冲突时重读重算
// 重试(上限 hygieneSaveAttempts 次)。管理员路径的整行替换语义不受影响;
// 管理员晚到提交复活已剥离目标的情况由下一次同步的卫生检查自愈。
func (s *Service) enforceRoutingHygieneAfterSync(ctx context.Context, operations OperationsRepository) error {
	for attempt := 0; ; attempt++ {
		config, err := operations.GetEgressOperationsConfig(ctx)
		if err != nil {
			return err
		}
		since := config.UpdatedAt
		originalDefault := config.DefaultTarget
		config.DefaultTarget = s.filterRoutingTarget(ctx, config.DefaultTarget)
		stripped := config.DefaultTarget != originalDefault
		for scope, target := range config.ScopeTargets {
			if filtered := s.filterRoutingTarget(ctx, target); !filtered.Configured() {
				delete(config.ScopeTargets, scope)
				stripped = true
			}
		}
		for class, target := range config.ClassTargets {
			if filtered := s.filterRoutingTarget(ctx, target); !filtered.Configured() {
				delete(config.ClassTargets, class)
				stripped = true
			}
		}
		if !stripped {
			return nil
		}
		var saveErr error
		if cas, ok := operations.(OperationsConfigCASWriter); ok {
			_, saveErr = cas.SaveEgressOperationsConfigIfCurrent(ctx, config, since)
			if errors.Is(saveErr, repository.ErrEgressConfigStale) && attempt+1 < hygieneSaveAttempts {
				continue
			}
		} else {
			_, saveErr = operations.SaveEgressOperationsConfig(ctx, config)
		}
		if saveErr != nil {
			return saveErr
		}
		s.invalidateOperationsConfig()
		return nil
	}
}

// filterRoutingTarget keeps a configured node target only while the node
// remains a valid fixed target; every other mode passes through unchanged.
func (s *Service) filterRoutingTarget(ctx context.Context, target domain.RoutingTarget) domain.RoutingTarget {
	if !target.Configured() || target.Mode.Normalized() != domain.RoutingTargetNode {
		return target
	}
	node, err := s.repository.GetEgressNode(ctx, target.NodeID)
	if err != nil || !domain.CanNodeServeFixedTarget(node) {
		return domain.RoutingTarget{}
	}
	if proxyURL, decryptErr := s.cipher.Decrypt(node.EncryptedProxyURL); decryptErr == nil && domain.IsAccountTemplateProxy(proxyURL) {
		return domain.RoutingTarget{}
	}
	return target
}

func sourceNextSyncAt(source domain.SubscriptionSource, now time.Time) time.Time {
	if source.RefreshIntervalSeconds < 60 {
		return now.Add(defaultProbeIntervalSeconds * time.Second)
	}
	return now.Add(time.Duration(source.RefreshIntervalSeconds) * time.Second)
}

func (s *Service) subscriptionFetchProxy(source domain.SubscriptionSource) (string, error) {
	encrypted := strings.TrimSpace(source.EncryptedProxyURL)
	if encrypted == "" {
		return "", nil
	}
	decrypted, err := s.cipher.Decrypt(encrypted)
	if err != nil {
		return "", err
	}
	normalized, err := NormalizeProxyURL(decrypted)
	if err != nil || normalized == "" || domain.IsAccountTemplateProxy(normalized) {
		return "", errors.New("订阅拉取代理配置无效")
	}
	return normalized, nil
}
