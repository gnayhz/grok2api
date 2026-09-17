package clientkey

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/pkg/tokenhash"
	portcrypto "github.com/chenyme/grok2api/backend/internal/port/crypto"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrInvalidKey         = errors.New("客户端 API Key 无效")
	ErrModelNotAllowed    = errors.New("客户端 API Key 无权使用该模型")
	ErrRateLimited        = errors.New("客户端 API Key 已超过 RPM 限制")
	ErrConcurrencyLimit   = errors.New("客户端 API Key 已达到并发上限")
	ErrBillingLimit       = errors.New("客户端 API Key 已达到用量上限")
	ErrRuntimeUnavailable = errors.New("运行态存储暂不可用")
)

// RateLimitedError 携带固定窗口剩余时间，供 429 响应渲染 Retry-After。
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

var (
	ErrInvalidFilter     = errors.New("客户端 Key 筛选条件无效")
	ErrInvalidInput      = errors.New("客户端 Key 参数无效")
	ErrNotFound          = errors.New("客户端 Key 不存在")
	ErrConflict          = errors.New("客户端 Key 冲突")
	ErrSecretUnavailable = errors.New("客户端 Key 明文不可用")
	ErrSystemManaged     = errors.New("系统托管 Client Key 不允许人工操作")
)

type CreateInput struct {
	Name                 string
	Enabled              bool
	ExpiresAt            *time.Time
	RPMLimit             int
	RPMUnlimited         bool
	MaxConcurrent        int
	ConcurrencyUnlimited bool
	BillingLimitUSDTicks int64
	AllowModelAliases    bool
	AllowedModels        []uint64
	ModelScope           *clientkeydomain.ModelScope
	ProviderScope        clientkeydomain.ProviderScope
	TierScope            clientkeydomain.TierScope
}

type UpdateInput = clientkeydomain.ManagementPatch

type Created struct {
	Key    clientkeydomain.Key
	Secret string
}

type ListFilter struct {
	Status     string
	ModelScope string
	Sort       repository.SortQuery
}

// Service 负责下游 API Key 创建、鉴权和调用限制。
type Service struct {
	keys          repository.ClientKeyRepository
	rateLimiter   repository.RateLimiter
	concurrency   repository.ConcurrencyLimiter
	defaultRPM    atomic.Int64
	defaultMax    atomic.Int64
	authCache     *authKeyCache
	touches       *touchTracker
	cipher        portcrypto.Cryptor
	tokens        portcrypto.TokenSource
	activeMu      sync.RWMutex
	activeBilling map[string]struct{}
	billingOwner  string
}

type billingReservationRepository interface {
	ReserveBillingUsage(ctx context.Context, id uint64, eventID string, amount int64, expiresAt time.Time, scope repository.BillingReservationScope) (bool, error)
	CancelBillingReservation(ctx context.Context, eventID string) error
	CleanupExpiredBillingReservations(ctx context.Context, now time.Time, limit int, scope repository.BillingReservationScope) (int, error)
}

type internalKeyInspector interface {
	CountInternalKeys(context.Context, []uint64) (int64, error)
}

func NewService(billingOwner string, keys repository.ClientKeyRepository, rateLimiter repository.RateLimiter, concurrency repository.ConcurrencyLimiter, defaultRPM, defaultMax int, cipher portcrypto.Cryptor, tokens portcrypto.TokenSource) *Service {
	service := &Service{billingOwner: strings.TrimSpace(billingOwner), keys: keys, rateLimiter: rateLimiter, concurrency: concurrency, authCache: newAuthKeyCache(), touches: newTouchTracker(), cipher: cipher, tokens: tokens, activeBilling: make(map[string]struct{})}
	service.UpdateDefaults(defaultRPM, defaultMax)
	return service
}

func (s *Service) UpdateDefaults(defaultRPM, defaultMax int) {
	s.defaultRPM.Store(int64(defaultRPM))
	s.defaultMax.Store(int64(defaultMax))
}

// ApplyInvalidation removes cached authorization policy after a local or remote
// client-key mutation. A zero ID represents a batch-wide invalidation.
func (s *Service) ApplyInvalidation(event repository.InvalidationEvent) {
	if event.Kind != repository.InvalidationClientKeyChanged {
		return
	}
	if event.ClientKeyID == 0 {
		s.authCache.clear()
		return
	}
	s.authCache.deleteID(event.ClientKeyID)
}

func (s *Service) List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]clientkeydomain.Key, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	if !slices.Contains([]string{"", "active", "disabled", "expired"}, filter.Status) || !slices.Contains([]string{"", "all", "restricted"}, filter.ModelScope) || !repository.IsValidSort(filter.Sort, "name", "prefix", "status", "rpmLimit", "maxConcurrent", "billingLimit", "expiresAt", "lastUsedAt") {
		return nil, 0, ErrInvalidFilter
	}
	if prefix, ok := clientkeydomain.SplitClientKey(strings.TrimSpace(search)); ok {
		search = prefix
	}
	return s.keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: search, Sort: filter.Sort}, Filter: repository.ClientKeyListFilter{Status: filter.Status, ModelScope: filter.ModelScope, Now: time.Now().UTC()}})
}

// Get returns one client key policy for trusted application services. It does
// not reveal the plaintext secret and must not be exposed by public handlers.
func (s *Service) Get(ctx context.Context, id uint64) (clientkeydomain.Key, error) {
	if id == 0 {
		return clientkeydomain.Key{}, ErrNotFound
	}
	value, err := s.keys.Get(ctx, id)
	return value, mapRepositoryError(err)
}

// Create 创建客户端 Key；哈希用于鉴权，加密副本仅供管理员按需再次复制。
func (s *Service) Create(ctx context.Context, input CreateInput) (Created, error) {
	if strings.TrimSpace(input.Name) == "" {
		return Created{}, invalidInput("Key 名称不能为空")
	}
	if input.RPMLimit < 0 || input.RPMLimit > clientkeydomain.MaxRPMLimit {
		return Created{}, invalidInput("rpmLimit 必须在 0 到 100000 之间")
	}
	if input.MaxConcurrent < 0 || input.MaxConcurrent > clientkeydomain.MaxConcurrent {
		return Created{}, invalidInput("maxConcurrent 必须在 0 到 1024 之间")
	}
	if input.BillingLimitUSDTicks < 0 || input.BillingLimitUSDTicks > clientkeydomain.MaxBillingLimitTicks {
		return Created{}, invalidInput("billingLimitUsdTicks 超出允许范围")
	}
	providerScope, providerScopeValid := clientkeydomain.NormalizeProviderScope(input.ProviderScope)
	tierScope, tierScopeValid := clientkeydomain.NormalizeTierScope(input.TierScope)
	if !providerScopeValid || !tierScopeValid {
		return Created{}, invalidInput("providerScope 或 tierScope 无效")
	}
	var requestedScope clientkeydomain.ModelScope
	if input.ModelScope != nil {
		requestedScope = *input.ModelScope
		if requestedScope == "" {
			return Created{}, invalidInput("modelScope 无效")
		}
	}
	modelScope, allowedModels, err := clientkeydomain.NormalizeModelAccess(requestedScope, input.AllowedModels)
	if err != nil {
		return Created{}, invalidInput(err.Error())
	}
	prefix, err := s.tokens.NewHexToken(6)
	if err != nil {
		return Created{}, err
	}
	secretPart, err := s.tokens.NewOpaqueToken(24)
	if err != nil {
		return Created{}, err
	}
	raw := clientkeydomain.FormatClientKey(prefix, secretPart)
	if s.cipher == nil {
		return Created{}, errors.New("客户端 Key 加密器未配置")
	}
	encryptedSecret, err := s.cipher.Encrypt(raw)
	if err != nil {
		return Created{}, fmt.Errorf("加密客户端 Key: %w", err)
	}
	if input.RPMUnlimited {
		input.RPMLimit = 0
	} else if input.RPMLimit == 0 {
		input.RPMLimit = int(s.defaultRPM.Load())
	}
	if input.ConcurrencyUnlimited {
		input.MaxConcurrent = 0
	} else if input.MaxConcurrent == 0 {
		input.MaxConcurrent = int(s.defaultMax.Load())
	}
	if input.RPMLimit < 0 || input.MaxConcurrent < 0 {
		return Created{}, invalidInput("RPM 和最大并发不能小于零")
	}
	value, err := s.keys.Create(ctx, clientkeydomain.Key{
		Name: strings.TrimSpace(input.Name), Prefix: prefix, SecretHash: tokenhash.HashToken(raw), EncryptedSecret: encryptedSecret,
		Enabled: input.Enabled, ExpiresAt: input.ExpiresAt, RPMLimit: input.RPMLimit, MaxConcurrent: input.MaxConcurrent,
		BillingLimitUSDTicks: input.BillingLimitUSDTicks, AllowModelAliases: input.AllowModelAliases, AllowedModels: allowedModels, ModelScope: modelScope,
		ProviderScope: providerScope, TierScope: tierScope,
	})
	if err != nil {
		return Created{}, mapRepositoryError(err)
	}
	// 注释承诺 Create 会失效负缓存：否则同前缀在 2s 窗口内仍 401。
	s.authCache.deletePrefix(prefix)
	return Created{Key: value, Secret: raw}, nil
}

// RevealSecret 解密指定客户端 Key，并校验密文、前缀和鉴权哈希仍然一致。
func (s *Service) RevealSecret(ctx context.Context, id uint64) (string, error) {
	value, err := s.keys.Get(ctx, id)
	if err != nil {
		return "", mapRepositoryError(err)
	}
	if value.InternalKind != "" {
		return "", ErrSystemManaged
	}
	if s.cipher == nil || value.EncryptedSecret == "" {
		return "", ErrSecretUnavailable
	}
	raw, err := s.cipher.Decrypt(value.EncryptedSecret)
	if err != nil {
		return "", fmt.Errorf("解密客户端 Key: %w", err)
	}
	prefix, ok := clientkeydomain.SplitClientKey(raw)
	if !ok || prefix != value.Prefix || subtle.ConstantTimeCompare([]byte(tokenhash.HashToken(raw)), []byte(value.SecretHash)) != 1 {
		return "", errors.New("客户端 Key 加密副本校验失败")
	}
	return raw, nil
}

func (s *Service) Update(ctx context.Context, id uint64, input UpdateInput) (clientkeydomain.Key, error) {
	value, err := s.keys.Get(ctx, id)
	if err != nil {
		return clientkeydomain.Key{}, mapRepositoryError(err)
	}
	if value.InternalKind != "" {
		return clientkeydomain.Key{}, ErrSystemManaged
	}
	patch, err := input.Normalize()
	if err != nil {
		return clientkeydomain.Key{}, invalidInput(err.Error())
	}
	updated, err := s.keys.Patch(ctx, id, patch)
	if err == nil {
		s.authCache.deleteID(id)
	}
	return updated, mapRepositoryError(err)
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	value, err := s.keys.Get(ctx, id)
	if err != nil {
		return mapRepositoryError(err)
	}
	if value.InternalKind != "" {
		return ErrSystemManaged
	}
	if err := s.keys.Delete(ctx, id); err != nil {
		return mapDeletionError(err)
	}
	s.touches.deleteID(id)
	s.authCache.deleteID(id)
	return nil
}

// BatchSetEnabled 批量启用或停用客户端 Key。
func (s *Service) BatchSetEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, err
	}
	if err := s.rejectInternalKeys(ctx, values); err != nil {
		return 0, err
	}
	updated, err := s.keys.UpdateManyEnabled(ctx, values, enabled)
	if err == nil {
		s.touches.deleteIDs(values)
		s.authCache.deleteIDs(values)
	}
	return updated, err
}

// BatchDelete 原子删除普通客户端 Key、关系与满足 M17 释放条件的作业/票据。
func (s *Service) BatchDelete(ctx context.Context, ids []uint64) (int64, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, err
	}
	if err := s.rejectInternalKeys(ctx, values); err != nil {
		return 0, err
	}
	deleted, err := s.keys.DeleteMany(ctx, values)
	if err == nil {
		s.touches.deleteIDs(values)
		s.authCache.deleteIDs(values)
	}
	return deleted, mapDeletionError(err)
}

// Authenticate 校验 API Key、RPM 和并发限制，并返回请求结束时必须调用的 release。
func (s *Service) Authenticate(ctx context.Context, raw string) (clientkeydomain.Key, func(), error) {
	prefix, ok := clientkeydomain.SplitClientKey(raw)
	if !ok {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	now := time.Now().UTC()
	lookup := s.authCache.lookup(prefix, now)
	if lookup.negative {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	value := lookup.value
	if !lookup.found {
		var err error
		value, err = s.keys.GetByPrefix(ctx, prefix)
		if err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				return clientkeydomain.Key{}, nil, fmt.Errorf("%w: 客户端 Key 仓储: %v", ErrRuntimeUnavailable, err)
			}
			s.authCache.putNegative(prefix, lookup.generation, now)
			return clientkeydomain.Key{}, nil, ErrInvalidKey
		}
		s.authCache.put(prefix, value, lookup.generation, now)
	}
	if value.InternalKind != "" {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	if !value.IsAvailable(now) {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	want := tokenhash.HashToken(raw)
	if subtle.ConstantTimeCompare([]byte(want), []byte(value.SecretHash)) != 1 {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	if value.BillingLimitUSDTicks > 0 {
		remaining := value.BillingLimitUSDTicks - value.BilledUsageUSDTicks
		if remaining <= 0 || value.ReservedUsageUSDTicks >= remaining {
			return clientkeydomain.Key{}, nil, ErrBillingLimit
		}
	}
	release := func() {}
	if value.MaxConcurrent > 0 {
		var acquired bool
		var err error
		// 并发租约先于 RPM 扣减:并发打满时拒绝的请求不该再消耗一次 RPM
		// 配额——高并发场景下有效 RPM 被虚耗, 实际吞吐低于配置预期。
		release, acquired, err = s.concurrency.Acquire(ctx, fmt.Sprintf("client:%d", value.ID), value.MaxConcurrent)
		if err != nil {
			return clientkeydomain.Key{}, nil, fmt.Errorf("%w: 并发租约: %v", ErrRuntimeUnavailable, err)
		}
		if !acquired {
			return clientkeydomain.Key{}, nil, ErrConcurrencyLimit
		}
	}
	if value.RPMLimit > 0 {
		allowed, retryAfter, err := s.rateLimiter.Allow(ctx, fmt.Sprintf("client:%d", value.ID), value.RPMLimit, now)
		if err != nil {
			release()
			return clientkeydomain.Key{}, nil, fmt.Errorf("%w: RPM 限流器: %v", ErrRuntimeUnavailable, err)
		}
		if !allowed {
			release()
			return clientkeydomain.Key{}, nil, &RateLimitedError{RetryAfter: retryAfter}
		}
	}
	if touchCtx, finish := s.touches.start(ctx, value.ID, now); finish != nil {
		go func(keyID uint64) {
			defer finish()
			_ = s.keys.Touch(touchCtx, keyID)
		}(value.ID)
	}
	return value, release, nil
}

func (s *Service) rejectInternalKeys(ctx context.Context, ids []uint64) error {
	inspector, ok := s.keys.(internalKeyInspector)
	if !ok {
		return nil
	}
	count, err := inspector.CountInternalKeys(ctx, ids)
	if err != nil {
		return err
	}
	if count > 0 {
		return ErrSystemManaged
	}
	return nil
}

// CanUseModel applies M08 model authorization independently of relation count.
func (s *Service) CanUseModel(value clientkeydomain.Key, modelID uint64) bool {
	return value.AllowsModel(modelID)
}

// ReserveBilling 为有限额 Key 原子预留本次请求的预计费用。
func (s *Service) ReserveBilling(ctx context.Context, key clientkeydomain.Key, eventID string, amount int64, ttl time.Duration) (bool, error) {
	if key.BillingLimitUSDTicks <= 0 || amount <= 0 {
		return false, nil
	}
	if s.billingOwner == "" || len(s.billingOwner) > 64 {
		return false, fmt.Errorf("%w: 持久计费预留缺少有效实例归属", ErrRuntimeUnavailable)
	}
	repo, ok := s.keys.(billingReservationRepository)
	if !ok {
		return false, fmt.Errorf("%w: 客户端 Key 仓储不支持计费预留", ErrRuntimeUnavailable)
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	reserved, err := repo.ReserveBillingUsage(ctx, key.ID, eventID, amount, time.Now().UTC().Add(ttl), s.billingScope())
	if errors.Is(err, repository.ErrLimitExceeded) {
		perfmetrics.Default.Inc("billing_reservation_total", perfmetrics.Labels{Subsystem: "billing", Operation: "reserve", Outcome: "limit_exceeded"})
		return false, ErrBillingLimit
	}
	if err != nil {
		perfmetrics.Default.Inc("billing_reservation_total", perfmetrics.Labels{Subsystem: "billing", Operation: "reserve", Outcome: "failed"})
		return false, fmt.Errorf("%w: 计费预留: %v", ErrRuntimeUnavailable, err)
	}
	if reserved {
		s.activeMu.Lock()
		s.activeBilling[eventID] = struct{}{}
		s.activeMu.Unlock()
	}
	perfmetrics.Default.Inc("billing_reservation_total", perfmetrics.Labels{Subsystem: "billing", Operation: "reserve", Outcome: "success"})
	return reserved, nil
}

// CancelBilling 释放未进入审计结算的计费预留。
func (s *Service) CancelBilling(ctx context.Context, eventID string) error {
	repo, ok := s.keys.(billingReservationRepository)
	if !ok {
		return nil
	}
	if err := repo.CancelBillingReservation(ctx, eventID); err != nil {
		perfmetrics.Default.Inc("billing_reservation_total", perfmetrics.Labels{Subsystem: "billing", Operation: "cancel", Outcome: "failed"})
		return fmt.Errorf("%w: 取消计费预留: %v", ErrRuntimeUnavailable, err)
	}
	s.CompleteBilling(eventID)
	perfmetrics.Default.Inc("billing_reservation_total", perfmetrics.Labels{Subsystem: "billing", Operation: "cancel", Outcome: "success"})
	return nil
}

// ProtectBillingBatch restores/retains the activity of durably accepted facts.
// M19 invokes it before its worker or startup cleanup can observe them.
func (s *Service) ProtectBillingBatch(eventIDs []string) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	for _, eventID := range eventIDs {
		if eventID != "" {
			s.activeBilling[eventID] = struct{}{}
		}
	}
}

// CompleteBilling removes the process-local active marker after the audit and
// billing transaction commits or the reservation is explicitly cancelled.
func (s *Service) CompleteBilling(eventID string) {
	if eventID == "" {
		return
	}
	s.CompleteBillingBatch([]string{eventID})
}

func (s *Service) CompleteBillingBatch(eventIDs []string) {
	if len(eventIDs) == 0 {
		return
	}
	s.activeMu.Lock()
	for _, eventID := range eventIDs {
		delete(s.activeBilling, eventID)
	}
	s.activeMu.Unlock()
}

func (s *Service) billingScope() repository.BillingReservationScope {
	s.activeMu.RLock()
	defer s.activeMu.RUnlock()
	protected := make([]string, 0, len(s.activeBilling))
	for eventID := range s.activeBilling {
		protected = append(protected, eventID)
	}
	return repository.BillingReservationScope{OwnerID: s.billingOwner, ProtectedEventIDs: protected}
}

// CleanupExpiredBilling 释放进程异常遗留的过期预留。
func (s *Service) CleanupExpiredBilling(ctx context.Context, limit int) (int, error) {
	if s.billingOwner == "" || len(s.billingOwner) > 64 {
		return 0, fmt.Errorf("%w: 持久计费预留缺少有效实例归属", ErrRuntimeUnavailable)
	}
	repo, ok := s.keys.(billingReservationRepository)
	if !ok {
		return 0, fmt.Errorf("%w: 客户端 Key 仓储不支持计费预留", ErrRuntimeUnavailable)
	}
	cleaned, err := repo.CleanupExpiredBillingReservations(ctx, time.Now().UTC(), limit, s.billingScope())
	outcome := "success"
	if err != nil {
		outcome = "failed"
	}
	perfmetrics.Default.Add("billing_reservation_cleanup_rows", perfmetrics.Labels{Subsystem: "billing", Operation: "cleanup", Outcome: outcome}, int64(cleaned))
	return cleaned, err
}

func normalizePage(page, pageSize int) (int, int) {
	return repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
}

func normalizeBatchIDs(ids []uint64) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, invalidInput("至少选择一个 Key")
	}
	if len(ids) > repository.MaxPageSize {
		return nil, invalidInput(fmt.Sprintf("单次最多处理 %d 个 Key", repository.MaxPageSize))
	}
	seen := make(map[uint64]struct{}, len(ids))
	result := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, invalidInput("Key ID 无效")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}

// invalidInput 为可安全返回给管理端的客户端 Key 参数错误附加稳定语义。
func invalidInput(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, message)
}

// mapRepositoryError 将仓储错误转换为客户端 Key 应用错误。
func mapRepositoryError(err error) error {
	if errors.Is(err, repository.ErrInvalidRecord) {
		return invalidInput("allowedModelIds 包含不存在或无效的模型")
	}
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, repository.ErrConflict) {
		return ErrConflict
	}
	return err
}

// Preserve M17's actionable reason while exposing M08's conflict contract.
func mapDeletionError(err error) error {
	for _, reason := range []error{media.ErrJobActive, media.ErrJobCompletionPending} {
		if errors.Is(err, reason) {
			return fmt.Errorf("%w: %w", ErrConflict, reason)
		}
	}
	return mapRepositoryError(err)
}
