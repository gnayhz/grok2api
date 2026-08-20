package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

var ErrSubscriptionSync = errors.New("代理订阅同步失败")

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
	userAgent := ""
	if source.Scope != domain.ScopeBuild {
		s.mu.RLock()
		userAgent = s.browserUA
		s.mu.RUnlock()
	}
	nodes := make([]domain.Node, 0, len(entries))
	for index, entry := range entries {
		encryptedProxy, encryptErr := s.cipher.Encrypt(entry.ProxyURL)
		if encryptErr != nil {
			recordFailure()
			return ImportResult{}, fmt.Errorf("%w: 加密导入节点", ErrSubscriptionSync)
		}
		nodes = append(nodes, domain.Node{
			Name: sourceNodeName(source.Name, index), Scope: source.Scope, Enabled: true,
			SourceID: source.ID, SourceKey: entry.Key, AccountCapacity: source.DefaultAccountCapacity,
			EncryptedProxyURL: encryptedProxy, UserAgent: userAgent, Health: 1, ProbeStatus: domain.ProbeStatusUnknown,
		})
	}
	imported, err := operations.UpsertEgressNodesFromSource(ctx, source.ID, nodes)
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	s.invalidateOperationsConfig()
	// Subscription upserts bypass the node-edit guard: a refreshed entry can
	// turn a rule fixed target into an account-bound template. Best effort -
	// a hygiene failure must not fail the sync that already committed.
	_ = s.enforceRouteRuleHygieneAfterSync(ctx, operations)
	if err := operations.UpdateEgressSourceSync(ctx, source.ID, now, nextSyncAt, imported, ""); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Imported: imported, Skipped: skipped}, nil
}

// enforceRouteRuleHygieneAfterSync strips enabled fixed route rules whose
// target can no longer serve them after a subscription sync. The repository
// hygiene inside the sync transaction cannot decrypt proxy URLs, so an entry
// that became an {account} template would otherwise keep routing a "fixed"
// exit that actually rotates per caller identity.
func (s *Service) enforceRouteRuleHygieneAfterSync(ctx context.Context, operations OperationsRepository) error {
	config, err := operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return err
	}
	kept := make([]domain.RouteRule, 0, len(config.RouteRules))
	changed := false
	for _, rule := range config.RouteRules {
		if !rule.Enabled || rule.TargetMode.Normalized() != domain.RouteRuleTargetFixed {
			kept = append(kept, rule)
			continue
		}
		node, err := s.repository.GetEgressNode(ctx, rule.TargetNodeID)
		valid := err == nil && domain.CanNodeServeFixedRouteTarget(node, rule.Scope)
		if valid {
			if proxyURL, decryptErr := s.cipher.Decrypt(node.EncryptedProxyURL); decryptErr == nil && strings.Contains(proxyURL, ProxyAccountPlaceholder) {
				valid = false
			}
		}
		if valid {
			kept = append(kept, rule)
			continue
		}
		changed = true
	}
	if !changed {
		return nil
	}
	config.RouteRules = kept
	if _, err := operations.SaveEgressOperationsConfig(ctx, config); err != nil {
		return err
	}
	s.invalidateOperationsConfig()
	return nil
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
	if err != nil || normalized == "" || strings.Contains(normalized, ProxyAccountPlaceholder) {
		return "", errors.New("订阅拉取代理配置无效")
	}
	return normalized, nil
}
