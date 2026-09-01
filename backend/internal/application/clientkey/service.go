package clientkey

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
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
	ProviderScope        clientkeydomain.ProviderScope
	TierScope            clientkeydomain.TierScope
}

type UpdateInput struct {
	Name                 *string
	Enabled              *bool
	ExpiresAt            *time.Time
	ClearExpiresAt       bool
	RPMLimit             *int
	MaxConcurrent        *int
	BillingLimitUSDTicks *int64
	AllowModelAliases    *bool
	AllowedModels        *[]uint64
	ProviderScope        *clientkeydomain.ProviderScope
	TierScope            *clientkeydomain.TierScope
}

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
	cipher        security.Cryptor
	activeMu      sync.RWMutex
	activeBilling map[string]struct{}
	// mediaJobs 可选：装配时注入。删除 client key 时预检活跃媒体作业
	//（media_jobs.client_key_id 是 ON DELETE RESTRICT 外键）并清理终态
	// 作业行——缺失时退回旧行为（依赖 FK 报错落 500）。
	mediaJobs repository.MediaJobRepository
}

type billingReservationRepository interface {
	ReserveBillingUsage(ctx context.Context, id uint64, eventID string, amount int64, expiresAt time.Time) (bool, error)
	CancelBillingReservation(ctx context.Context, eventID string) error
	CleanupExpiredBillingReservations(ctx context.Context, now time.Time, limit int, protectedEventIDs ...[]string) (int, error)
}

type internalKeyInspector interface {
	CountInternalKeys(context.Context, []uint64) (int64, error)
}

func NewService(keys repository.ClientKeyRepository, rateLimiter repository.RateLimiter, concurrency repository.ConcurrencyLimiter, defaultRPM, defaultMax int, cipher security.Cryptor) *Service {
	service := &Service{keys: keys, rateLimiter: rateLimiter, concurrency: concurrency, authCache: newAuthKeyCache(), touches: newTouchTracker(), cipher: cipher, activeBilling: make(map[string]struct{})}
	service.UpdateDefaults(defaultRPM, defaultMax)
	return service
}

// SetMediaJobRepository 注入媒体作业仓储（可选依赖）：使 BatchDelete 能以
// 可操作的冲突错误替代 FK RESTRICT 落下的裸 500（round 51）。
func (s *Service) SetMediaJobRepository(mediaJobs repository.MediaJobRepository) {
	s.mediaJobs = mediaJobs
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
	if !validListFilter(filter.Status, "", "active", "disabled", "expired") || !validListFilter(filter.ModelScope, "", "all", "restricted") || !repository.IsValidSort(filter.Sort, "name", "prefix", "status", "rpmLimit", "maxConcurrent", "billingLimit", "expiresAt", "lastUsedAt") {
		return nil, 0, ErrInvalidFilter
	}
	if prefix, ok := security.SplitClientKey(strings.TrimSpace(search)); ok {
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

func validListFilter(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
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
	prefix, err := security.NewHexToken(6)
	if err != nil {
		return Created{}, err
	}
	secretPart, err := security.NewOpaqueToken(24)
	if err != nil {
		return Created{}, err
	}
	raw := security.FormatClientKey(prefix, secretPart)
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
		Name: strings.TrimSpace(input.Name), Prefix: prefix, SecretHash: security.HashToken(raw), EncryptedSecret: encryptedSecret,
		Enabled: input.Enabled, ExpiresAt: input.ExpiresAt, RPMLimit: input.RPMLimit, MaxConcurrent: input.MaxConcurrent,
		BillingLimitUSDTicks: input.BillingLimitUSDTicks, AllowModelAliases: input.AllowModelAliases, AllowedModels: input.AllowedModels,
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
	prefix, ok := security.SplitClientKey(raw)
	if !ok || prefix != value.Prefix || subtle.ConstantTimeCompare([]byte(security.HashToken(raw)), []byte(value.SecretHash)) != 1 {
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
	if input.Name != nil {
		value.Name = strings.TrimSpace(*input.Name)
		if value.Name == "" {
			return clientkeydomain.Key{}, invalidInput("Key 名称不能为空")
		}
	}
	if input.Enabled != nil {
		value.Enabled = *input.Enabled
	}
	if input.ClearExpiresAt {
		value.ExpiresAt = nil
	} else if input.ExpiresAt != nil {
		value.ExpiresAt = input.ExpiresAt
	}
	if input.RPMLimit != nil {
		if *input.RPMLimit < 0 || *input.RPMLimit > clientkeydomain.MaxRPMLimit {
			return clientkeydomain.Key{}, invalidInput("rpmLimit 必须在 0 到 100000 之间")
		}
		value.RPMLimit = *input.RPMLimit
	}
	if input.MaxConcurrent != nil {
		if *input.MaxConcurrent < 0 || *input.MaxConcurrent > clientkeydomain.MaxConcurrent {
			return clientkeydomain.Key{}, invalidInput("maxConcurrent 必须在 0 到 1024 之间")
		}
		value.MaxConcurrent = *input.MaxConcurrent
	}
	if input.BillingLimitUSDTicks != nil {
		if *input.BillingLimitUSDTicks < 0 || *input.BillingLimitUSDTicks > clientkeydomain.MaxBillingLimitTicks {
			return clientkeydomain.Key{}, invalidInput("billingLimitUsdTicks 超出允许范围")
		}
		value.BillingLimitUSDTicks = *input.BillingLimitUSDTicks
	}
	if input.AllowModelAliases != nil {
		value.AllowModelAliases = *input.AllowModelAliases
	}
	if input.AllowedModels != nil {
		value.AllowedModels = *input.AllowedModels
	}
	if input.ProviderScope != nil {
		providerScope, valid := clientkeydomain.NormalizeProviderScope(*input.ProviderScope)
		if !valid {
			return clientkeydomain.Key{}, invalidInput("providerScope 无效")
		}
		value.ProviderScope = providerScope
	}
	if input.TierScope != nil {
		tierScope, valid := clientkeydomain.NormalizeTierScope(*input.TierScope)
		if !valid {
			return clientkeydomain.Key{}, invalidInput("tierScope 无效")
		}
		value.TierScope = tierScope
	}
	updated, err := s.keys.Update(ctx, value)
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
		return mapRepositoryError(err)
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

// BatchDelete 原子删除客户端 Key 及其模型权限。
func (s *Service) BatchDelete(ctx context.Context, ids []uint64) (int64, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, err
	}
	if err := s.rejectInternalKeys(ctx, values); err != nil {
		return 0, err
	}
	if s.mediaJobs != nil {
		// media_jobs.client_key_id 是 ON DELETE RESTRICT：作业行存在时
		// DeleteMany 被 FK 拒绝并落裸 500。活跃作业给出可操作的冲突；
		// 终态作业只剩归档价值（审计行有 client_key_name 快照），随删。
		if active, activeErr := s.mediaJobs.CountActiveMediaJobsByClientKeys(ctx, values); activeErr != nil {
			return 0, activeErr
		} else if active > 0 {
			return 0, fmt.Errorf("%w: %d 个媒体作业仍在执行（queued/in_progress），请先等待完成或清理后再删除该 Key", ErrConflict, active)
		}
		if _, terminalErr := s.mediaJobs.DeleteTerminalMediaJobsByClientKeys(ctx, values); terminalErr != nil {
			return 0, terminalErr
		}
	}
	deleted, err := s.keys.DeleteMany(ctx, values)
	if err == nil {
		s.touches.deleteIDs(values)
		s.authCache.deleteIDs(values)
	}
	return deleted, err
}

// Authenticate 校验 API Key、RPM 和并发限制，并返回请求结束时必须调用的 release。
func (s *Service) Authenticate(ctx context.Context, raw string) (clientkeydomain.Key, func(), error) {
	prefix, ok := security.SplitClientKey(raw)
	if !ok {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	now := time.Now().UTC()
	if s.authCache.getNegative(prefix, now) {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	value, cached := s.authCache.get(prefix, now)
	if !cached {
		var err error
		value, err = s.keys.GetByPrefix(ctx, prefix)
		if err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				return clientkeydomain.Key{}, nil, fmt.Errorf("%w: 客户端 Key 仓储: %v", ErrRuntimeUnavailable, err)
			}
			s.authCache.putNegative(prefix, now)
			return clientkeydomain.Key{}, nil, ErrInvalidKey
		}
		s.authCache.put(prefix, value, now)
	}
	if value.InternalKind != "" {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	if !value.IsAvailable(now) {
		return clientkeydomain.Key{}, nil, ErrInvalidKey
	}
	want := security.HashToken(raw)
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
	if s.touches.shouldTouch(value.ID, now) {
		// lastUsedAt 仅服务管理面展示;同步 UPDATE 出现在请求路径(每 key
		// 每分钟一次)是不必要的串行点。移出请求生命周期:断开连接不打断
		// 簿记(WithoutCancel),丢失一次 Touch 对功能无影响。
		touchCtx, touchCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		go func(keyID uint64) {
			defer touchCancel()
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

// CanUseModel 判断空权限列表代表全部模型，否则要求显式授权。
func (s *Service) CanUseModel(value clientkeydomain.Key, modelID uint64) bool {
	return value.AllowsModel(modelID)
}

// ReserveBilling 为有限额 Key 原子预留本次请求的预计费用。
func (s *Service) ReserveBilling(ctx context.Context, key clientkeydomain.Key, eventID string, amount int64, ttl time.Duration) (bool, error) {
	if key.BillingLimitUSDTicks <= 0 || amount <= 0 {
		return false, nil
	}
	repo, ok := s.keys.(billingReservationRepository)
	if !ok {
		return false, fmt.Errorf("%w: 客户端 Key 仓储不支持计费预留", ErrRuntimeUnavailable)
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	reserved, err := repo.ReserveBillingUsage(ctx, key.ID, eventID, amount, time.Now().UTC().Add(ttl))
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

// CompleteBilling removes the process-local active marker after the audit and
// billing transaction commits or the reservation is explicitly cancelled.
func (s *Service) CompleteBilling(eventID string) {
	if eventID == "" {
		return
	}
	s.CompleteBillingBatch([]string{eventID})
}

func (s *Service) CompleteBillingBatch(eventIDs []string) {
	s.ReleaseBillingProtectionBatch(eventIDs)
}

// ReleaseBillingProtectionBatch removes process-local activity markers. The
// durable reservation remains authoritative until commit, cancel, or expiry.
func (s *Service) ReleaseBillingProtectionBatch(eventIDs []string) {
	if len(eventIDs) == 0 {
		return
	}
	s.activeMu.Lock()
	for _, eventID := range eventIDs {
		delete(s.activeBilling, eventID)
	}
	s.activeMu.Unlock()
}

// CleanupExpiredBilling 释放进程异常遗留的过期预留。
func (s *Service) CleanupExpiredBilling(ctx context.Context, limit int) (int, error) {
	repo, ok := s.keys.(billingReservationRepository)
	if !ok {
		return 0, fmt.Errorf("%w: 客户端 Key 仓储不支持计费预留", ErrRuntimeUnavailable)
	}
	s.activeMu.RLock()
	protected := make([]string, 0, len(s.activeBilling))
	for eventID := range s.activeBilling {
		protected = append(protected, eventID)
	}
	s.activeMu.RUnlock()
	cleaned, err := repo.CleanupExpiredBillingReservations(ctx, time.Now().UTC(), limit, protected)
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
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, repository.ErrConflict) {
		return ErrConflict
	}
	return err
}
