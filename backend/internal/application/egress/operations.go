package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	defaultProbeIntervalSeconds = 900
	maxManualProbeNodes         = 200
	maxConcurrentProbes         = 8
)

var ErrOperationsUnavailable = errors.New("代理运营功能不可用")

// OperationsRepository is deliberately optional. Existing egress consumers
// still only need the narrow routing repository while relational persistence
// provides this richer administrative surface.
type OperationsRepository interface {
	ListEgressSources(context.Context) ([]domain.SubscriptionSource, error)
	ListEgressSourcePage(context.Context, repository.EgressSourceListQuery) ([]domain.SubscriptionSource, int64, error)
	ListDueEgressSources(context.Context, time.Time, int) ([]domain.SubscriptionSource, error)
	GetEgressSource(context.Context, uint64) (domain.SubscriptionSource, error)
	CreateEgressSource(context.Context, domain.SubscriptionSource) (domain.SubscriptionSource, error)
	UpdateEgressSource(context.Context, domain.SubscriptionSource) (domain.SubscriptionSource, error)
	DeleteEgressSource(context.Context, uint64) error
	UpdateEgressSourceSync(context.Context, uint64, time.Time, time.Time, int, string) error
	UpsertEgressNodesFromSource(context.Context, uint64, []domain.Node) (int, error)
	CreateEgressNodes(context.Context, []domain.Node) (int, error)
	UpdateEgressNodeProbe(context.Context, uint64, string, domain.ProbeResult) error
	ListDueEgressNodes(context.Context, time.Time, time.Duration, int) ([]domain.Node, error)
	GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error)
	SaveEgressOperationsConfig(context.Context, domain.OperationsConfig) (domain.OperationsConfig, error)
}

// NodeProber is implemented by the infrastructure egress manager. Its fixed
// probe endpoint prevents admin input from controlling the outbound target.
type NodeProber interface {
	ProbeEgressNode(context.Context, domain.Node) (domain.ProbeResult, error)
}

// OperationsConfigCASWriter is the optional compare-and-write capability for
// operations-config writers whose snapshot may race concurrent administrators.
// Background writers (subscription-sync hygiene) must use it so a stale
// snapshot can never overwrite a concurrent admin commit; repositories without
// it fall back to the unconditional save with unchanged-snapshot skip.
type OperationsConfigCASWriter interface {
	SaveEgressOperationsConfigIfCurrent(ctx context.Context, value domain.OperationsConfig, since time.Time) (domain.OperationsConfig, error)
}

type OperationsConfigInvalidator interface {
	InvalidateOperationsConfig()
}

// PoolCacheInvalidator drops the infrastructure manager's pool/member/rotation
// caches. Pool configuration changes must not wait out the snapshot TTL to
// become visible to scheduling.
type PoolCacheInvalidator interface {
	InvalidatePoolCache()
}

type SubscriptionSourceInput struct {
	Name                   string
	Enabled                bool
	URL                    *string
	ClearURL               bool
	ProxyURL               *string
	ClearProxyURL          bool
	RefreshIntervalSeconds *int
}

type ImportInput struct {
	Name    string
	Content string
}

type ImportResult struct {
	Imported int
	Skipped  int
}

type ProbeBatchResult struct {
	Requested int
	Healthy   int
	Unhealthy int
}

// OperationsConfigInput is the administrative update payload. Routing
// fields are pointers so a nil keeps the stored value (sparse updates).
type OperationsConfigInput struct {
	ProbeProvider        domain.ProbeProvider
	ProbeIntervalSeconds int
	DefaultTarget        *RoutingTargetInput
	ScopeTargets         map[domain.Scope]RoutingTargetInput
	ClassTargets         map[domain.TrafficClass]RoutingTargetInput
}

// RoutingTargetInput is one routing decision. Mode "auto" resets the level
// to follow the next level down (a scope target of auto follows 总出口).
type RoutingTargetInput struct {
	Mode   domain.RoutingTargetMode
	NodeID uint64
	PoolID uint64
}

func (input RoutingTargetInput) toDomain() domain.RoutingTarget {
	return domain.RoutingTarget{Mode: input.Mode, NodeID: input.NodeID, PoolID: input.PoolID}
}

func (s *Service) operationsRepository() (OperationsRepository, error) {
	if s == nil || s.operations == nil {
		return nil, ErrOperationsUnavailable
	}
	return s.operations, nil
}

func (s *Service) SetNodeProber(value NodeProber) {
	s.mu.Lock()
	s.prober = value
	s.mu.Unlock()
}

func (s *Service) SetOperationsConfigInvalidator(value OperationsConfigInvalidator) {
	s.mu.Lock()
	s.operationsCache = value
	s.mu.Unlock()
}

// SetPoolCacheInvalidator wires the infrastructure pool cache flush. The
// manager implements both invalidator interfaces; pools must flush their own
// cache because pool rows and memberships do not ride the operations config.
func (s *Service) SetPoolCacheInvalidator(value PoolCacheInvalidator) {
	s.mu.Lock()
	s.poolCache = value
	s.mu.Unlock()
}

func (s *Service) invalidatePoolCache() {
	s.mu.RLock()
	value := s.poolCache
	s.mu.RUnlock()
	if value != nil {
		value.InvalidatePoolCache()
	}
}

func (s *Service) invalidateOperationsConfig() {
	s.mu.RLock()
	value := s.operationsCache
	s.mu.RUnlock()
	if value != nil {
		value.InvalidateOperationsConfig()
	}
}

func (s *Service) nodeProber() NodeProber {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.prober
}

func (s *Service) ListSources(ctx context.Context) ([]domain.PublicSubscriptionSource, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return nil, err
	}
	values, err := operations.ListEgressSources(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]domain.PublicSubscriptionSource, 0, len(values))
	for _, value := range values {
		result = append(result, publicSource(value))
	}
	return result, nil
}

func (s *Service) ListSourcePage(ctx context.Context, page, pageSize int, search string) ([]domain.PublicSubscriptionSource, int64, error) {
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	operations, err := s.operationsRepository()
	if err != nil {
		return nil, 0, err
	}
	values, total, err := operations.ListEgressSourcePage(ctx, repository.EgressSourceListQuery{
		Page: repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: strings.TrimSpace(search)},
	})
	if err != nil {
		return nil, 0, err
	}
	result := make([]domain.PublicSubscriptionSource, 0, len(values))
	for _, value := range values {
		result = append(result, publicSource(value))
	}
	return result, total, nil
}

func (s *Service) CreateSource(ctx context.Context, input SubscriptionSourceInput) (domain.PublicSubscriptionSource, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	value, err := s.applySourceInput(domain.SubscriptionSource{}, input, true)
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	created, err := operations.CreateEgressSource(ctx, value)
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	return publicSource(created), nil
}

func (s *Service) UpdateSource(ctx context.Context, id uint64, input SubscriptionSourceInput) (domain.PublicSubscriptionSource, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	value, err := operations.GetEgressSource(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicSubscriptionSource{}, ErrNotFound
	}
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	value, err = s.applySourceInput(value, input, false)
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	updated, err := operations.UpdateEgressSource(ctx, value)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicSubscriptionSource{}, ErrNotFound
	}
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	return publicSource(updated), nil
}

func (s *Service) DeleteSource(ctx context.Context, id uint64) error {
	operations, err := s.operationsRepository()
	if err != nil {
		return err
	}
	err = operations.DeleteEgressSource(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *Service) SyncSource(ctx context.Context, id uint64) (ImportResult, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return ImportResult{}, err
	}
	source, err := operations.GetEgressSource(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ImportResult{}, ErrNotFound
	}
	if err != nil {
		return ImportResult{}, err
	}
	return s.syncSource(ctx, operations, source)
}

func (s *Service) ImportText(ctx context.Context, input ImportInput) (ImportResult, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return ImportResult{}, err
	}
	if err := validateImportInput(input); err != nil {
		return ImportResult{}, err
	}
	entries, skipped, err := parseProxySubscription(input.Content)
	if err != nil {
		return ImportResult{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	nodes := make([]domain.Node, 0, len(entries))
	for index, entry := range entries {
		encryptedProxy, encryptErr := s.cipher.Encrypt(entry.ProxyURL)
		if encryptErr != nil {
			return ImportResult{}, encryptErr
		}
		nodes = append(nodes, domain.Node{
			Name: sourceNodeName(input.Name, index), Enabled: true,
			EncryptedProxyURL: encryptedProxy, Health: 1,
			ProbeStatus: domain.ProbeStatusUnknown,
		})
	}
	created, err := operations.CreateEgressNodes(ctx, nodes)
	if err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Imported: created, Skipped: skipped}, nil
}

func (s *Service) TestNode(ctx context.Context, id uint64) (domain.ProbeResult, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.ProbeResult{}, err
	}
	node, err := s.repository.GetEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.ProbeResult{}, ErrNotFound
	} else if err != nil {
		return domain.ProbeResult{}, err
	}
	prober := s.nodeProber()
	if prober == nil {
		return domain.ProbeResult{}, ErrOperationsUnavailable
	}
	result, probeErr := prober.ProbeEgressNode(ctx, node)
	if result.TestedAt.IsZero() {
		result.TestedAt = time.Now().UTC()
	}
	if !result.Status.IsValid() {
		result.Status = domain.ProbeStatusUnhealthy
	}
	if probeErr != nil {
		result.Status = domain.ProbeStatusUnhealthy
		if strings.TrimSpace(result.Error) == "" {
			result.Error = "代理探测失败"
		}
	}
	if updateErr := operations.UpdateEgressNodeProbe(ctx, id, node.EncryptedProxyURL, result); updateErr != nil {
		if errors.Is(updateErr, repository.ErrNotFound) {
			return result, ErrNotFound
		}
		if errors.Is(updateErr, repository.ErrConflict) {
			return result, ErrProbeStale
		}
		return result, updateErr
	}
	// An unreachable proxy is a completed probe with an unhealthy result, not
	// an API operation failure. Persistence and repository failures still return
	// above so callers can distinguish them from node health.
	return result, nil
}

func (s *Service) TestNodes(ctx context.Context, ids []uint64) (ProbeBatchResult, error) {
	if len(ids) == 0 {
		nodes, err := s.repository.ListEgressNodes(ctx, repository.SortQuery{})
		if err != nil {
			return ProbeBatchResult{}, err
		}
		ids = make([]uint64, 0, len(nodes))
		for _, node := range nodes {
			if node.Enabled && node.EncryptedProxyURL != "" {
				ids = append(ids, node.ID)
			}
		}
	}
	ids = uniqueIDs(ids)
	if len(ids) > maxManualProbeNodes {
		return ProbeBatchResult{}, fmt.Errorf("%w: 单次最多测试 %d 个代理", ErrInvalidInput, maxManualProbeNodes)
	}
	result := ProbeBatchResult{Requested: len(ids)}
	if len(ids) == 0 {
		return result, nil
	}
	var mu sync.Mutex
	jobs := make(chan uint64)
	var workers sync.WaitGroup
	for range min(maxConcurrentProbes, len(ids)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				probe, err := s.TestNode(ctx, id)
				mu.Lock()
				if err == nil && probe.Status == domain.ProbeStatusHealthy {
					result.Healthy++
				} else {
					result.Unhealthy++
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return result, ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	return result, nil
}

func (s *Service) OperationsConfig(ctx context.Context) (domain.OperationsConfig, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.OperationsConfig{}, err
	}
	return operations.GetEgressOperationsConfig(ctx)
}

// UpdateOperationsConfig replaces probe scheduling and routing. Nil routing
// levels keep their stored value; an explicit auto target resets that level.
func (s *Service) UpdateOperationsConfig(ctx context.Context, input OperationsConfigInput) (domain.OperationsConfig, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.OperationsConfig{}, err
	}
	if input.ProbeIntervalSeconds < 60 || input.ProbeIntervalSeconds > 86400 {
		return domain.OperationsConfig{}, fmt.Errorf("%w: 节点检测间隔必须在 60 到 86400 秒之间", ErrInvalidInput)
	}
	current, err := operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return domain.OperationsConfig{}, err
	}
	probeProvider := input.ProbeProvider
	if probeProvider == "" {
		probeProvider = current.ProbeProvider.Normalized()
	}
	if !probeProvider.IsValid() {
		return domain.OperationsConfig{}, fmt.Errorf("%w: 不支持的代理探测服务", ErrInvalidInput)
	}
	config := domain.OperationsConfig{
		ProbeProvider: probeProvider, ProbeIntervalSeconds: input.ProbeIntervalSeconds,
		DefaultTarget: current.DefaultTarget, ScopeTargets: current.ScopeTargets, ClassTargets: current.ClassTargets,
	}
	if input.DefaultTarget != nil {
		config.DefaultTarget = input.DefaultTarget.toDomain()
	}
	if input.ScopeTargets != nil {
		config.ScopeTargets = make(map[domain.Scope]domain.RoutingTarget, len(input.ScopeTargets))
		for scope, target := range input.ScopeTargets {
			config.ScopeTargets[scope] = target.toDomain()
		}
	}
	if input.ClassTargets != nil {
		config.ClassTargets = make(map[domain.TrafficClass]domain.RoutingTarget, len(input.ClassTargets))
		for class, target := range input.ClassTargets {
			config.ClassTargets[class] = target.toDomain()
		}
	}
	if err := domain.ValidateRoutingTargets(config.DefaultTarget, config.ScopeTargets, config.ClassTargets); err != nil {
		return domain.OperationsConfig{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.validateRoutingTargets(ctx, config); err != nil {
		return domain.OperationsConfig{}, err
	}
	config.UpdatedAt = time.Now().UTC()
	saved, err := operations.SaveEgressOperationsConfig(ctx, config)
	if errors.Is(err, repository.ErrEgressRoutingNodeInUse) {
		return domain.OperationsConfig{}, fmt.Errorf("%w: 出口路由的固定目标节点必须保持启用且可用", ErrInvalidInput)
	}
	if errors.Is(err, repository.ErrEgressRoutingInvalid) {
		return domain.OperationsConfig{}, fmt.Errorf("%w: 出口路由目标代理池不存在", ErrInvalidInput)
	}
	if err == nil {
		s.invalidateOperationsConfig()
	}
	return saved, err
}

// validateRoutingTargets checks that referenced nodes and pools exist and
// remain schedulable. Sticky per-account proxy templates are rejected: they
// rotate their exit with the caller identity, contradicting a fixed target.
func (s *Service) validateRoutingTargets(ctx context.Context, config domain.OperationsConfig) error {
	nodeIDs := make(map[uint64]struct{})
	poolIDs := make(map[uint64]struct{})
	collect := func(target domain.RoutingTarget) {
		switch target.Mode.Normalized() {
		case domain.RoutingTargetNode:
			nodeIDs[target.NodeID] = struct{}{}
		case domain.RoutingTargetPool:
			poolIDs[target.PoolID] = struct{}{}
		}
	}
	collect(config.DefaultTarget)
	for _, target := range config.ScopeTargets {
		collect(target)
	}
	for _, target := range config.ClassTargets {
		collect(target)
	}
	for id := range nodeIDs {
		node, err := s.repository.GetEgressNode(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: 出口路由的固定目标节点不存在", ErrInvalidInput)
		}
		if err := s.validateFixedTargetNode(node); err != nil {
			return err
		}
	}
	for id := range poolIDs {
		if s.poolsRepository() == nil {
			return fmt.Errorf("%w: 代理池存储不可用", ErrOperationsUnavailable)
		}
		if _, err := s.poolsRepository().GetEgressPool(ctx, id); err != nil {
			return fmt.Errorf("%w: 出口路由目标代理池不存在", ErrInvalidInput)
		}
	}
	return nil
}

func (s *Service) applySourceInput(value domain.SubscriptionSource, input SubscriptionSourceInput, create bool) (domain.SubscriptionSource, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return domain.SubscriptionSource{}, fmt.Errorf("%w: 订阅名称必须在 1 到 160 个字符之间", ErrInvalidInput)
	}
	value.Name, value.Enabled = name, input.Enabled
	if input.RefreshIntervalSeconds != nil {
		if *input.RefreshIntervalSeconds < 60 || *input.RefreshIntervalSeconds > 86400 {
			return domain.SubscriptionSource{}, fmt.Errorf("%w: 订阅刷新间隔必须在 60 到 86400 秒之间", ErrInvalidInput)
		}
		value.RefreshIntervalSeconds = *input.RefreshIntervalSeconds
	}
	if value.RefreshIntervalSeconds == 0 {
		value.RefreshIntervalSeconds = defaultProbeIntervalSeconds
	}
	if input.ClearURL {
		value.EncryptedURL = ""
	} else if input.URL != nil {
		urlValue, err := normalizeSubscriptionURL(*input.URL)
		if err != nil {
			return domain.SubscriptionSource{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		encryptedURL, err := s.cipher.Encrypt(urlValue)
		if err != nil {
			return domain.SubscriptionSource{}, err
		}
		value.EncryptedURL = encryptedURL
	}
	if create && value.EncryptedURL == "" {
		return domain.SubscriptionSource{}, fmt.Errorf("%w: 必须提供订阅地址", ErrInvalidInput)
	}
	if input.ClearProxyURL {
		value.EncryptedProxyURL = ""
	} else if input.ProxyURL != nil {
		proxyURL, err := NormalizeProxyURL(*input.ProxyURL)
		if err != nil {
			return domain.SubscriptionSource{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		if proxyURL == "" {
			return domain.SubscriptionSource{}, fmt.Errorf("%w: 订阅代理地址不能为空", ErrInvalidInput)
		}
		if domain.IsAccountTemplateProxy(proxyURL) {
			return domain.SubscriptionSource{}, fmt.Errorf("%w: 订阅代理地址不能包含账号占位符", ErrInvalidInput)
		}
		encryptedProxyURL, err := s.cipher.Encrypt(proxyURL)
		if err != nil {
			return domain.SubscriptionSource{}, err
		}
		value.EncryptedProxyURL = encryptedProxyURL
	}
	// 配置变更时的调度重置(next_sync_at 清空、last_sync_error 清空)由持久层
	// 在 UPDATE 语句内对当前行原子判定(仅当订阅地址/拉取代理真的变化)——
	// 应用层快照不再写这两列,避免读-改-写窗口回滚维护循环的同步进度。
	return value, nil
}

func publicSource(value domain.SubscriptionSource) domain.PublicSubscriptionSource {
	return domain.PublicSubscriptionSource{
		ID: value.ID, Name: value.Name, Enabled: value.Enabled, URLConfigured: value.EncryptedURL != "",
		ProxyConfigured:        value.EncryptedProxyURL != "",
		RefreshIntervalSeconds: value.RefreshIntervalSeconds,
		LastSyncedAt:           value.LastSyncedAt, NextSyncAt: value.NextSyncAt, LastSyncImported: value.LastSyncImported, LastSyncError: value.LastSyncError,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func validateImportInput(input ImportInput) error {
	if strings.TrimSpace(input.Name) == "" || len(strings.TrimSpace(input.Name)) > 150 || strings.TrimSpace(input.Content) == "" {
		return fmt.Errorf("%w: 批量导入参数无效", ErrInvalidInput)
	}
	return nil
}

// SourceURL reveals the stored subscription URL so operators can edit or
// clear it later; the list API only exposes a configured flag. Write-only
// remains the rule for ordinary listings.
func (s *Service) SourceURL(ctx context.Context, id uint64) (string, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return "", err
	}
	source, err := operations.GetEgressSource(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(source.EncryptedURL) == "" {
		return "", fmt.Errorf("%w: 订阅未配置地址", ErrInvalidInput)
	}
	urlValue, err := s.cipher.Decrypt(source.EncryptedURL)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(urlValue), nil
}

// SourceProxyURL reveals the stored fetch proxy URL for the same reason.
func (s *Service) SourceProxyURL(ctx context.Context, id uint64) (string, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return "", err
	}
	source, err := operations.GetEgressSource(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(source.EncryptedProxyURL) == "" {
		return "", fmt.Errorf("%w: 订阅未配置拉取代理", ErrInvalidInput)
	}
	proxyURL, err := s.cipher.Decrypt(source.EncryptedProxyURL)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(proxyURL), nil
}
