package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/cfcookies"
	"github.com/chenyme/grok2api/backend/internal/pkg/proxyurl"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrInvalidInput         = errors.New("代理节点参数无效")
	ErrInvalidFilter        = errors.New("出口代理筛选条件无效")
	ErrInvalidSort          = errors.New("代理节点排序条件无效")
	ErrNotFound             = errors.New("代理节点不存在")
	ErrProbeStale           = errors.New("代理配置在探测期间已更新，请重新测试")
	ErrClearanceUnavailable = errors.New("Clearance 刷新不可用")
)

const (
	maxProxyURLBytes    = 8192
	maxRotationURLBytes = 8192
	// 代理账号模板占位符与判定收敛于 domain,此处仅做兼容别名。
	ProxyAccountPlaceholder = domain.ProxyAccountPlaceholder
	proxyAccountSentinel    = "grok2api_account_placeholder"
)

// Input is the create/update payload for one egress node. Nodes are pure
// proxy resources: which traffic they serve is decided by routing only.
type Input struct {
	Name             string
	Enabled          bool
	ProxyPool        *bool
	ProxyURL         *string
	ClearProxyURL    bool
	RotationURL      *string
	ClearRotationURL bool
	// RotationEnabled pauses/resumes the stored webhook without wiping it.
	RotationEnabled *bool
}

type ListFilter struct {
	Enabled     string
	ProbeStatus string
	Sort        repository.SortQuery
}

type ServiceRepository interface {
	repository.EgressRepository
	repository.EgressNodePageRepository
}

type Service struct {
	repository      ServiceRepository
	operations      OperationsRepository
	cipher          security.Cryptor
	mu              sync.RWMutex
	clearance       ClearanceManager
	prober          NodeProber
	operationsCache OperationsConfigInvalidator
	poolCache       PoolCacheInvalidator

	// Exit-IP quality guard state (quality.go / rotation.go).
	qualityQuarantiner QualityQuarantiner
	qualityGuard       QualityGuardConfig
	qualityLogger      *slog.Logger
	qualityProber      EgressQualityProber
	qualityMu          sync.Mutex
	qualityEvidence    map[uint64][]degradeObservation

	// 死出口确认状态(probe_dead.go): 连续双族探活失败的观测计数。
	probeDeadMu sync.Mutex
	probeDead   map[uint64]probeDeadObservation

	rotationCfg    RotationConfig
	rotation       *rotationScheduler
	rotationLogger *slog.Logger
}

type UnhealthyCleanupPreview struct {
	Nodes               int64
	SubscriptionManaged int64
}

// BatchNodeDeleter is optional so lightweight repository adapters only need
// the single-node contract unless they can provide an atomic bulk operation.
type BatchNodeDeleter interface {
	DeleteEgressNodes(context.Context, []uint64) (int, error)
}

type BatchNodeEnabledUpdater interface {
	UpdateEgressNodesEnabled(context.Context, []uint64, bool) (int, error)
}

type ClearanceManager interface {
	RefreshClearance(context.Context, uint64) error
	ForgetClearance(uint64)
}

type BatchClearanceManager interface {
	ForgetClearances([]uint64)
}

func NewService(storage ServiceRepository, cipher security.Cryptor) *Service {
	// qualityEvidence 必须在构造时初始化:OnEgressDegraded 首次写入时 map 为
	// nil 会 panic, 生产(app.go)与测试直构 Service 字面量的行为从此一致。
	service := &Service{repository: storage, cipher: cipher, qualityEvidence: map[uint64][]degradeObservation{}}
	if operations, ok := storage.(OperationsRepository); ok {
		service.operations = operations
	}
	return service
}

func (s *Service) SetClearanceManager(value ClearanceManager) {
	s.mu.Lock()
	s.clearance = value
	s.mu.Unlock()
}

func (s *Service) List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]domain.PublicNode, int64, error) {
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	if !validListValue(filter.Enabled, "enabled", "disabled") ||
		!validListValue(filter.ProbeStatus, string(domain.ProbeStatusHealthy), string(domain.ProbeStatusUnhealthy), string(domain.ProbeStatusUnknown)) {
		return nil, 0, ErrInvalidFilter
	}
	if !repository.IsValidSort(filter.Sort, "name", "proxy", "health") {
		return nil, 0, ErrInvalidSort
	}
	var enabled *bool
	if filter.Enabled != "" {
		value := filter.Enabled == "enabled"
		enabled = &value
	}
	values, total, err := s.repository.ListEgressNodePage(ctx, repository.EgressNodeListQuery{
		Page: repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: strings.TrimSpace(search), Sort: filter.Sort},
		Filter: repository.EgressNodeListFilter{
			Enabled: enabled, ProbeStatus: domain.ProbeStatus(filter.ProbeStatus),
		},
	})
	if err != nil {
		return nil, 0, err
	}
	return s.publicNodes(ctx, values), total, nil
}

func (s *Service) ListAll(ctx context.Context, sort repository.SortQuery) ([]domain.PublicNode, error) {
	if !repository.IsValidSort(sort, "name", "proxy", "health") {
		return nil, ErrInvalidSort
	}
	values, err := s.repository.ListEgressNodes(ctx, sort)
	if err != nil {
		return nil, err
	}
	return s.publicNodes(ctx, values), nil
}

func (s *Service) publicNodes(ctx context.Context, values []domain.Node) []domain.PublicNode {
	poolNames := s.egressPoolNameMap(ctx)
	result := make([]domain.PublicNode, 0, len(values))
	for _, value := range values {
		result = append(result, s.publicNode(value, poolNames))
	}
	return result
}

func validListValue(value string, allowed ...string) bool {
	if value == "" {
		return true
	}
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (s *Service) Create(ctx context.Context, input Input) (domain.PublicNode, error) {
	value, err := s.applyInput(domain.Node{}, input, true)
	if err != nil {
		return domain.PublicNode{}, err
	}
	created, err := s.repository.CreateEgressNode(ctx, value)
	if err == nil {
		s.forgetClearance(created.ID)
	}
	return s.publicNode(created, s.egressPoolNameMap(ctx)), err
}

func (s *Service) Update(ctx context.Context, id uint64, input Input) (domain.PublicNode, error) {
	value, err := s.repository.GetEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicNode{}, ErrNotFound
	}
	if err != nil {
		return domain.PublicNode{}, err
	}
	value, err = s.applyInput(value, input, false)
	if err != nil {
		return domain.PublicNode{}, err
	}
	if err := s.validateRoutingTargetNodeUpdate(ctx, value); err != nil {
		return domain.PublicNode{}, err
	}
	updated, err := s.repository.UpdateEgressNode(ctx, value)
	if err == nil {
		s.forgetClearance(updated.ID)
	}
	return s.publicNode(updated, s.egressPoolNameMap(ctx)), err
}

// ProxyURL returns one administrator-selected secret without placing it in
// ordinary list/detail payloads. HTTP handlers must mark the response no-store.
func (s *Service) ProxyURL(ctx context.Context, id uint64) (string, error) {
	value, err := s.repository.GetEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return "", fmt.Errorf("%w: 节点未配置代理地址", ErrInvalidInput)
	}
	proxyURL, err := s.cipher.Decrypt(value.EncryptedProxyURL)
	if err != nil {
		return "", err
	}
	return NormalizeProxyURL(proxyURL)
}

// RotationURL reveals the stored rotation webhook so operators can edit or
// clear it later; the list API only exposes a configured flag.
func (s *Service) RotationURL(ctx context.Context, id uint64) (string, error) {
	value, err := s.repository.GetEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value.EncryptedRotationURL) == "" {
		return "", fmt.Errorf("%w: 节点未配置换 IP Webhook", ErrInvalidInput)
	}
	rotationURL, err := s.cipher.Decrypt(value.EncryptedRotationURL)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(rotationURL), nil
}

// validateRoutingTargetNodeUpdate keeps a node serving as a fixed routing
// target schedulable for that role after an edit. Disabling it or clearing
// its proxy would otherwise silently degrade the routing decision to the
// automatic schedule with no administrator-visible error.
func (s *Service) validateRoutingTargetNodeUpdate(ctx context.Context, node domain.Node) error {
	if s.operations == nil {
		return nil
	}
	config, err := s.operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return err
	}
	references := func(target domain.RoutingTarget) bool {
		return target.Mode.Normalized() == domain.RoutingTargetNode && target.NodeID == node.ID
	}
	if references(config.DefaultTarget) {
		if err := s.validateFixedTargetNode(node); err != nil {
			return fmt.Errorf("节点已是总出口的固定目标，无法应用当前修改: %w", err)
		}
		return nil
	}
	for scope, target := range config.ScopeTargets {
		if references(target) {
			if err := s.validateFixedTargetNode(node); err != nil {
				return fmt.Errorf("节点已是 %s 作用域出口的固定目标，无法应用当前修改: %w", scope, err)
			}
			return nil
		}
	}
	for class, target := range config.ClassTargets {
		if references(target) {
			if err := s.validateFixedTargetNode(node); err != nil {
				return fmt.Errorf("节点已是 %s 流量类别出口的固定目标，无法应用当前修改: %w", class, err)
			}
			return nil
		}
	}
	return nil
}

// validateFixedTargetNode reports whether the node can keep serving as a
// fixed routing target. 旋转出口(代理池模式)可以:固定的是隧道而非瞬时
// 出口 IP。粘性账号模板仍拒绝——它按调用方账号渲染不同子会话,对路由
// 而言"节点身份"本身不稳定,应进池用 affinity 策略。
func (s *Service) validateFixedTargetNode(node domain.Node) error {
	if !domain.CanNodeServeFixedTarget(node) {
		return fmt.Errorf("%w: 固定出口目标必须启用且已配置代理地址", ErrInvalidInput)
	}
	if proxyURL, err := s.cipher.Decrypt(node.EncryptedProxyURL); err == nil && domain.IsAccountTemplateProxy(proxyURL) {
		return fmt.Errorf("%w: 固定出口目标不能使用账号代理模板", ErrInvalidInput)
	}
	return nil
}

// UpdateManyEnabled changes only the scheduling state, leaving proxy secrets,
// health, and probes untouched.
func (s *Service) UpdateManyEnabled(ctx context.Context, nodeIDs []uint64, enabled bool) (int, error) {
	ids := uniqueIDs(nodeIDs)
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: 代理节点参数无效", ErrInvalidInput)
	}
	if batch, ok := s.repository.(BatchNodeEnabledUpdater); ok {
		updated, err := batch.UpdateEgressNodesEnabled(ctx, ids, enabled)
		if errors.Is(err, repository.ErrEgressRoutingNodeInUse) {
			return 0, fmt.Errorf("%w: 出口路由的固定目标节点不能被禁用", ErrInvalidInput)
		}
		if err != nil {
			return 0, err
		}
		if updated > 0 {
			s.forgetClearances(ids)
		}
		return updated, nil
	}

	updated := 0
	for _, id := range ids {
		node, err := s.repository.GetEgressNode(ctx, id)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return updated, err
		}
		if node.Enabled == enabled {
			continue
		}
		node.Enabled = enabled
		if !enabled {
			if err := s.validateRoutingTargetNodeUpdate(ctx, node); err != nil {
				return updated, err
			}
		}
		if _, err := s.repository.UpdateEgressNode(ctx, node); err != nil {
			return updated, err
		}
		s.forgetClearance(id)
		updated++
	}
	return updated, nil
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	err := s.repository.DeleteEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err == nil {
		s.forgetClearance(id)
		s.invalidateOperationsConfig()
	}
	return err
}

// DeleteMany removes nodes in one repository operation when available.
func (s *Service) DeleteMany(ctx context.Context, nodeIDs []uint64) (int, error) {
	ids := uniqueIDs(nodeIDs)
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: 代理节点参数无效", ErrInvalidInput)
	}
	if batch, ok := s.repository.(BatchNodeDeleter); ok {
		deleted, err := batch.DeleteEgressNodes(ctx, ids)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			s.forgetClearance(id)
		}
		s.invalidateOperationsConfig()
		return deleted, nil
	}

	deleted := 0
	for _, id := range ids {
		if err := s.Delete(ctx, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func (s *Service) PreviewUnhealthyCleanup(ctx context.Context) (UnhealthyCleanupPreview, error) {
	if cleaner, ok := s.repository.(repository.EgressNodeUnhealthyCleaner); ok {
		value, err := cleaner.PreviewUnhealthyEgressNodes(ctx)
		return UnhealthyCleanupPreview{
			Nodes: value.Nodes, SubscriptionManaged: value.SubscriptionManaged,
		}, err
	}
	values, err := s.repository.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		return UnhealthyCleanupPreview{}, err
	}
	result := UnhealthyCleanupPreview{}
	for _, value := range values {
		if value.IPv4Probe.Status != domain.ProbeStatusUnhealthy || value.IPv6Probe.Status != domain.ProbeStatusUnhealthy {
			continue
		}
		result.Nodes++
		if value.SourceID != 0 {
			result.SubscriptionManaged++
		}
	}
	return result, nil
}

func (s *Service) DeleteUnhealthy(ctx context.Context) (int, error) {
	if cleaner, ok := s.repository.(repository.EgressNodeUnhealthyCleaner); ok {
		ids, err := cleaner.DeleteUnhealthyEgressNodes(ctx)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			s.forgetClearance(id)
		}
		if len(ids) > 0 {
			s.invalidateOperationsConfig()
		}
		return len(ids), nil
	}
	values, err := s.repository.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		return 0, err
	}
	ids := make([]uint64, 0)
	for _, value := range values {
		if value.IPv4Probe.Status == domain.ProbeStatusUnhealthy && value.IPv6Probe.Status == domain.ProbeStatusUnhealthy {
			ids = append(ids, value.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return s.DeleteMany(ctx, ids)
}

func (s *Service) RefreshClearance(ctx context.Context, id uint64) error {
	if _, err := s.repository.GetEgressNode(ctx, id); errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager == nil {
		return ErrClearanceUnavailable
	}
	return manager.RefreshClearance(ctx, id)
}

func uniqueIDs(values []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(values))
	result := make([]uint64, 0, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (s *Service) forgetClearance(id uint64) {
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager != nil {
		manager.ForgetClearance(id)
	}
}

func (s *Service) forgetClearances(ids []uint64) {
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager == nil {
		return
	}
	if batch, ok := manager.(BatchClearanceManager); ok {
		batch.ForgetClearances(ids)
		return
	}
	for _, id := range ids {
		manager.ForgetClearance(id)
	}
}

func (s *Service) applyInput(value domain.Node, input Input, create bool) (domain.Node, error) {
	proxyPool := value.ProxyPool
	if input.ProxyPool != nil {
		proxyPool = *input.ProxyPool
	}
	configurationChanged := create || value.ProxyPool != proxyPool || (!value.Enabled && input.Enabled) || input.ClearProxyURL || input.ProxyURL != nil
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return domain.Node{}, fmt.Errorf("%w: 名称必须在 1 到 160 个字符之间", ErrInvalidInput)
	}
	value.Name, value.Enabled, value.ProxyPool = name, input.Enabled, proxyPool
	if input.ClearProxyURL {
		value.EncryptedProxyURL = ""
		value.ProxyPool = false
	} else if input.ProxyURL != nil {
		normalized, err := NormalizeProxyURL(*input.ProxyURL)
		if err != nil {
			return domain.Node{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		if normalized != "" {
			value.EncryptedProxyURL, err = s.cipher.Encrypt(normalized)
			if err != nil {
				return domain.Node{}, err
			}
		}
	}
	if value.ProxyPool && strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return domain.Node{}, fmt.Errorf("%w: 代理池模式需要配置代理地址", ErrInvalidInput)
	}
	// 换 IP webhook：完整 URL（含节点侧 token）加密存储，调用即重启隧道换出口。
	// 先处理地址增删，再应用开关：同一次请求里 URL 与 enabled=true 同时到达时，
	// 开关不能被"地址尚未写入"的守卫误复位。
	if input.ClearRotationURL {
		value.EncryptedRotationURL = ""
	} else if input.RotationURL != nil {
		normalized := strings.TrimSpace(*input.RotationURL)
		if normalized != "" {
			parsed, parseErr := url.Parse(normalized)
			if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return domain.Node{}, fmt.Errorf("%w: 换 IP webhook 必须是 http(s) URL", ErrInvalidInput)
			}
			if len(normalized) > maxRotationURLBytes {
				return domain.Node{}, fmt.Errorf("%w: 换 IP webhook 过长", ErrInvalidInput)
			}
			encrypted, encryptErr := s.cipher.Encrypt(normalized)
			if encryptErr != nil {
				return domain.Node{}, encryptErr
			}
			value.EncryptedRotationURL = encrypted
		}
	}
	if input.RotationEnabled != nil {
		if *input.RotationEnabled && strings.TrimSpace(value.EncryptedProxyURL) == "" && strings.TrimSpace(value.EncryptedRotationURL) == "" {
			return domain.Node{}, fmt.Errorf("%w: 开启换 IP 前需要先填写 Webhook 地址", ErrInvalidInput)
		}
		value.RotationEnabled = *input.RotationEnabled
	}
	if value.EncryptedRotationURL == "" {
		value.RotationEnabled = false
	}
	if configurationChanged {
		value.Health = 1
		value.FailureCount = 0
		value.CooldownUntil = nil
		value.LastError = ""
		value.ProbeStatus = domain.ProbeStatusUnknown
		value.LastProbedAt = nil
		value.ProbeLatencyMS = 0
		value.ExitIP = ""
		value.ProbeError = ""
		value.ProbeProvider = ""
		value.IPv4Probe = domain.ProbeFamilyResult{Status: domain.ProbeStatusUnknown}
		value.IPv6Probe = domain.ProbeFamilyResult{Status: domain.ProbeStatusUnknown}
	}
	// Any administrator edit invalidates freshness. Keep the binding fingerprint:
	// managed mode may use the existing cookie as last-known-good only when the
	// target and actual proxy still match the binding that produced it.
	value.ClearanceRefreshedAt = nil
	value.ClearanceFingerprint = ""
	return value, nil
}

func (s *Service) publicNode(value domain.Node, poolNames map[uint64]string) domain.PublicNode {
	proxyDisplay, proxyFingerprint, accountTemplate := s.proxyMetadata(value.EncryptedProxyURL)
	proxyPool := value.ProxyPool || accountTemplate
	health, failureCount, cooldownUntil, lastError := value.Health, value.FailureCount, value.CooldownUntil, value.LastError
	if proxyPool {
		health, failureCount, cooldownUntil, lastError = 1, 0, nil, ""
	}
	return domain.PublicNode{
		ID: value.ID, Name: value.Name, Enabled: value.Enabled,
		ProxyConfigured: value.EncryptedProxyURL != "", ProxyDisplay: proxyDisplay, ProxyFingerprint: proxyFingerprint,
		ProxyPool:          proxyPool,
		RotationConfigured: value.EncryptedRotationURL != "", RotationEnabled: value.EncryptedRotationURL != "" && value.RotationEnabled, LastRotatedAt: value.LastRotatedAt,
		RotationAttempts: value.RotationAttempts, LastRotationError: value.LastRotationError,
		DegradeCount: value.DegradeCount, LastDegradedAt: value.LastDegradedAt,
		SourceID:          value.SourceID,
		SourceName:        value.SourceName,
		Pools:             nodePoolRefs(value.PoolIDs, poolNames),
		AccountBoundProxy: accountTemplate,
		Health:            health, FailureCount: failureCount, CooldownUntil: cooldownUntil, LastError: lastError,
		ProbeStatus: value.ProbeStatus, LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		ProbeProvider: value.ProbeProvider,
		IPv4Probe:     value.IPv4Probe, IPv6Probe: value.IPv6Probe,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

// egressPoolNameMap loads pool names once per listing call.
func (s *Service) egressPoolNameMap(ctx context.Context) map[uint64]string {
	names := map[uint64]string{}
	store, err := s.poolStore()
	if err != nil {
		return names
	}
	pools, listErr := store.ListEgressPools(ctx)
	if listErr != nil {
		return names
	}
	for _, pool := range pools {
		names[pool.ID] = pool.Name
	}
	return names
}

// nodePoolRefs maps pool ids onto {id,name} refs; names may be nil.
func nodePoolRefs(ids []uint64, names map[uint64]string) []domain.NodePoolRef {
	if len(ids) == 0 {
		return nil
	}
	refs := make([]domain.NodePoolRef, 0, len(ids))
	for _, id := range ids {
		ref := domain.NodePoolRef{ID: id}
		if names != nil {
			ref.Name = names[id]
		}
		refs = append(refs, ref)
	}
	return refs
}

func (s *Service) proxyMetadata(encrypted string) (string, string, bool) {
	if s == nil || s.cipher == nil || strings.TrimSpace(encrypted) == "" {
		return "", "", false
	}
	proxyURL, err := s.cipher.Decrypt(encrypted)
	if err != nil {
		return "", "", false
	}
	proxyURL, err = NormalizeProxyURL(proxyURL)
	if err != nil || proxyURL == "" {
		return "", "", false
	}
	return ProxyDisplay(proxyURL), security.HashToken(proxyURL)[:12], domain.IsAccountTemplateProxy(proxyURL)
}

// ProxyDisplay preserves the routable endpoint and, for standard proxies, the
// username, while ensuring passwords and tunnel credentials never enter list
// responses. The short fingerprint lets operators identify duplicate physical
// proxies without revealing the secret.
func ProxyDisplay(proxyURL string) string {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return ""
	}
	if tunnelproxy.IsSupportedScheme(parsed.Scheme) {
		config, parseErr := tunnelproxy.Parse(proxyURL)
		if parseErr != nil {
			return ""
		}
		return strings.ToLower(config.Scheme) + "://***@" + config.Server
	}
	if parsed.Host == "" {
		return ""
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(username, "***")
		} else {
			parsed.User = url.User(username)
		}
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", "", ""
	return parsed.String()
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

// NormalizeProxyURL delegates to pkg/proxyurl: the implementation moved to
// a neutral package so infra no longer imports the application layer
// (dependency direction is strictly downward again).
func NormalizeProxyURL(value string) (string, error) {
	return proxyurl.NormalizeProxyURL(value)
}

// SanitizeCloudflareCookies 委托 pkg/cfcookies:实现移至中立包, 账号层
// 不再需要为净化 Cookie 导入出口应用包(业务与代理解耦)。
func SanitizeCloudflareCookies(value string) string {
	return cfcookies.Sanitize(value)
}
