package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/cfcookies"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/pkg/tokenhash"
	portcrypto "github.com/chenyme/grok2api/backend/internal/port/crypto"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrDevicePending       = errors.New("Device OAuth 等待用户授权")
	ErrDeviceSlowDown      = errors.New("Device OAuth 轮询过快")
	ErrDeviceDenied        = errors.New("Device OAuth 已拒绝或过期")
	ErrInvalidFilter       = errors.New("账号筛选条件无效")
	ErrInvalidInput        = errors.New("账号参数无效")
	ErrInvalidImport       = errors.New("账号凭据格式无效")
	ErrImportLimit         = errors.New("导入账号数量超过限制")
	ErrExportLimit         = errors.New("导出账号数量超过限制")
	ErrNotFound            = errors.New("账号不存在")
	ErrUnsupported         = errors.New("账号来源不支持该操作")
	ErrConversionBusy      = errors.New("账号正在转换为 Grok Build")
	ErrConflict            = errors.New("账号操作存在冲突")
	ErrAccountPoolMismatch = errors.New("批量操作包含不属于当前号池的账号")
)

var ErrCredentialRefreshPermanent = errors.New("OAuth refresh token 已永久失效")
var errQuotaRefreshBusy = errors.New("额度同步已由其他实例执行")

type RateLimitReconcileState string

const (
	RateLimitReconcileInconclusive RateLimitReconcileState = "inconclusive"
	RateLimitReconcileRefreshing   RateLimitReconcileState = "refreshing"
	RateLimitReconcileAvailable    RateLimitReconcileState = "available"
	RateLimitReconcileExhausted    RateLimitReconcileState = "exhausted"
)

const (
	// estimatedFreeTokenLimit is only a fallback until an upstream exhaustion
	// response supplies the account-specific actual/limit pair.
	estimatedFreeTokenLimit     int64         = 500_000
	freeUsageWindow             time.Duration = 24 * time.Hour
	forcedRefreshMinInterval    time.Duration = 30 * time.Second
	credentialRefreshAdvance    time.Duration = 3 * time.Minute
	credentialRefreshSafetyPoll time.Duration = time.Minute
	credentialRefreshTimeout    time.Duration = 30 * time.Second
	credentialRefreshStateTTL   time.Duration = 5 * time.Second
	credentialStateWriteTimeout time.Duration = 5 * time.Second

	credentialRefreshBatchSize  = 100
	credentialRefreshMaxBatches = 50

	managedTaskWorkerCeiling = 50
	quotaRefreshQueueSize    = 4096
	quotaRefreshTimeout      = 30 * time.Second
	quotaRefreshDirtyTTL     = 24 * time.Hour
	quotaRefreshPollInterval = 500 * time.Millisecond
	quotaRefreshSharedPoll   = time.Second
	quotaRefreshBackoffBase  = time.Second
	// quotaRefreshBackoffMax：失败重试的退避上限。历史值 1 分钟在含死凭据/
	// 被拒账号的 fleet 下构成重试风暴——每个永远失败的账号以 ~1 分钟一轮的
	// 节奏重试，死凭据规模即可打满工人池（历史线上实测：持续高强度的
	// 无效上游请求）。提到 30 分钟，并配合
	// quotaRefreshFailureBudget 熔断停靠。
	quotaRefreshBackoffMax = 30 * time.Minute
	// quotaRefreshFailureBudget：同一 (account,mode) 连续失败达到该值后熔断
	// 停靠——保留该轮需求的失败预算，不再自动重试；外部再次显式入队
	//（请求路径 429 核实 / 迁移任务 / 巡检）会开启全新 episode（failures
	// 归零，见 QueueQuotaRefresh），重试预算按 episode 有界。
	quotaRefreshFailureBudget                     = 8
	consoleQuotaRefreshMinInterval                = 30 * time.Second
	observedModelPersistInterval                  = 30 * time.Minute
	observedModelLocalCacheTTL                    = 5 * time.Second
	observedModelLockShards                       = 64
	maxCredentialExportAccounts                   = 10000
	maxCredentialImportAccounts                   = 10000
	credentialImportChunkSize                     = 100
	credentialImportPrepareWorkers                = 3
	maxQuotaResetAccounts                         = 10000
	quotaResetChunkSize                           = 500
	maxBatchUpdateAccounts                        = 10000
	maxBuildConversionAccounts                    = 1000
	maxWebConsoleSyncAccounts                     = 1000
	accountTaskBatchSize                          = 1000
	buildBotFlagCacheTTL            time.Duration = 30 * time.Second
	linkedDeleteRuntimeCleanupLimit               = 3 * time.Second
)

const permanentRefreshExpiredReason = "OAuth refresh token 已永久失效且 access token 已过期"
const buildBotFlagCacheKey = "build-bot-flagged-account-ids"

type buildBotFlagIndexRepository interface {
	ListBuildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error)
	ListBuildBotFlagCredentialBatch(ctx context.Context, afterID uint64, limit int) ([]repository.BuildBotFlagCredential, error)
	UpdateBuildBotFlagSources(ctx context.Context, values []repository.BuildBotFlagSourceUpdate) error
	CountBuildBotFlagged(ctx context.Context) (int64, error)
	CountAvailableBuildBotFlagged(ctx context.Context, now time.Time) (int64, error)
}

type observedModelState struct {
	model       string
	persistedAt time.Time
}

type observedModelShard struct {
	sync.Mutex
	values        map[uint64]observedModelState
	lastCleanupAt time.Time
}

type QuotaRefreshStats struct {
	Pending int
	Queued  int
	Running int
}

type QuotaType string
type QuotaStatus string

const (
	QuotaTypeUnknown        QuotaType   = "unknown"
	QuotaTypeFree           QuotaType   = "free"
	QuotaTypePaid           QuotaType   = "paid"
	QuotaStatusActive       QuotaStatus = "active"
	QuotaStatusWaitingReset QuotaStatus = "waitingReset"
	QuotaStatusProbing      QuotaStatus = "probing"
)

type QuotaView struct {
	Type            QuotaType
	Source          string
	Confidence      string
	Unit            string
	Used            float64
	Limit           float64
	Remaining       float64
	UsagePercent    float64
	LimitKnown      bool
	WindowHours     int
	Observed        bool
	Confirmed       bool
	Status          QuotaStatus
	PeriodStart     string
	PeriodEnd       string
	ExhaustedAt     *time.Time
	NextProbeAt     *time.Time
	LastConfirmedAt *time.Time
}

type View struct {
	Quality            *QualityState
	Credential         accountdomain.Credential
	Billing            *accountdomain.Billing
	Quota              QuotaView
	QuotaWindows       []accountdomain.QuotaWindow
	BuildBotFlagged    bool
	BuildBotFlagSource int
	// EnabledChanged is request-scoped update metadata. It is not persisted or
	// serialized directly; the HTTP layer uses it to avoid warning when a PATCH
	// merely repeats the account's existing enabled value.
	EnabledChanged bool
}

type DeviceStartResult struct {
	SessionID               string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresAt               time.Time
}

// BatchProgressObserver 在单个账号任务结束后报告批次完成数。
type BatchProgressObserver func(completed, total int) error

// Service 负责 OAuth 账号接入、刷新、额度和持久化生命周期。
// CredentialRejectionClassifier 把上游状态/错误体解释为凭据拒绝事实。
// 方言级 JSON/text 解释在 Provider 边界实现,经组合根注入;消费方只用事实。
// ResponsesProbeInspector 解释账号可用性检测的原生 Responses 结果
// (拒绝事实/生成完成);方言级 JSON 规则在 Provider 边界,经组合根注入。
type ResponsesProbeInspector interface {
	InspectResponsesProbe(status int, body io.Reader) (provider.CredentialRejection, error)
}

type CredentialRejectionClassifier interface {
	ClassifyCredentialRejection(status int, body []byte, err error) provider.CredentialRejection
}

type Service struct {
	qualityStates func(context.Context) (map[uint64]QualityState, error)
	// rateLimiter 拥有 Team 限流观察状态:互斥/活跃/过期/窗口表/身份映射。
	rateLimiter         *teamRateLimitTracker
	accounts            repository.AccountRepository
	audits              repository.AuditRepository
	deviceSessions      repository.DeviceSessionRepository
	sticky              repository.StickySessionRepository
	refreshLock         repository.DistributedLock
	concurrency         repository.ConcurrencyLimiter
	quotaQueue          repository.QuotaRecoveryQueue
	quotaRefreshState   repository.QuotaRefreshCoordinator
	providers           provider.Registry
	cipher              portcrypto.Cryptor
	tokens              portcrypto.TokenSource
	rejections          CredentialRejectionClassifier
	probeInspect        ResponsesProbeInspector
	billingSyncs        OperationGroup[string]
	quotaSyncs          OperationGroup[string]
	identitySyncs       OperationGroup[accountdomain.CredentialRef]
	observedModelWrites OperationGroup[string]
	observedModelStore  repository.ObservedModelStateRepository
	observedModelShards [observedModelLockShards]observedModelShard
	quotaRefresh        *quotaRefreshTracker
	quotaRefreshCursor  uint64
	quotaDurableScan    uint64
	conversionPool      *batch.Pool
	syncPool            *batch.Pool
	refreshPool         *batch.Pool
	// detectPool 专用于管理端「检测账号」，与额度同步/续期隔离，默认并发 32。
	detectPool          *batch.Pool
	credentialLifecycle *credentialLifecycle
	maintenance         *maintenancePolicy
	buildBotFlagCache   *resultcache.Cache[string, []uint64]
	logger              *slog.Logger
	now                 func() time.Time
}

func (s *Service) SetQuotaRecoveryQueue(queue repository.QuotaRecoveryQueue) {
	s.quotaQueue = queue
}

func (s *Service) SetQuotaRefreshCoordinator(value repository.QuotaRefreshCoordinator) {
	s.quotaRefreshState = value
}

func (s *Service) QuotaRefreshStats() QuotaRefreshStats {
	s.quotaRefresh.mu.Lock()
	defer s.quotaRefresh.mu.Unlock()
	result := QuotaRefreshStats{}
	for _, state := range s.quotaRefresh.obs {
		if state == nil {
			continue
		}
		if state.pending || state.queued || state.running {
			result.Pending++
		}
		if state.queued {
			result.Queued++
		}
		if state.running {
			result.Running++
		}
	}
	return result
}

// SetConcurrencyLimiter 让账号维护任务读取与推理路由相同的活动租约。
func (s *Service) SetConcurrencyLimiter(value repository.ConcurrencyLimiter) {
	s.concurrency = value
}

// SetObservedModelStore enables best-effort cross-instance duplicate suppression.
func (s *Service) SetObservedModelStore(value repository.ObservedModelStateRepository) {
	s.observedModelStore = value
}

func NewService(accounts repository.AccountRepository, audits repository.AuditRepository, deviceSessions repository.DeviceSessionRepository, sticky repository.StickySessionRepository, providers provider.Registry, cipher portcrypto.Cryptor, tokens portcrypto.TokenSource, rejections CredentialRejectionClassifier, probeInspector ResponsesProbeInspector, refreshLock repository.DistributedLock) *Service {
	return &Service{
		rateLimiter: newTeamRateLimitTracker(),
		accounts:    accounts, audits: audits, deviceSessions: deviceSessions, sticky: sticky,
		providers: providers, cipher: cipher, tokens: tokens, rejections: rejections, probeInspect: probeInspector, refreshLock: refreshLock,
		quotaRefresh:        &quotaRefreshTracker{obs: make(map[string]*quotaRefreshState), queue: make(chan quotaRefreshRequest, quotaRefreshQueueSize), wake: make(chan struct{}, 1)},
		credentialLifecycle: newCredentialLifecycle(),
		maintenance:         newMaintenancePolicy(),
		buildBotFlagCache:   resultcache.New[string, []uint64](1, buildBotFlagCacheTTL),
		conversionPool:      batch.NewPool(25), syncPool: batch.NewPool(25), refreshPool: batch.NewPool(25), detectPool: batch.NewPool(32),
		logger: slog.Default(),
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) SetBulkPool(pool *batch.Pool) {
	if pool != nil {
		s.conversionPool, s.syncPool, s.refreshPool = pool, pool, pool
	}
}

// SetTaskPools 为转换、同步和凭据刷新绑定独立分类并发池。
func (s *Service) SetTaskPools(conversion, syncPool, refresh *batch.Pool) {
	if conversion != nil {
		s.conversionPool = conversion
	}
	if syncPool != nil {
		s.syncPool = syncPool
	}
	if refresh != nil {
		s.refreshPool = refresh
	}
}

// SetDetectPool 绑定管理端「检测账号」专用并发池；nil 时保留现有池。
func (s *Service) SetDetectPool(pool *batch.Pool) {
	if pool != nil {
		s.detectPool = pool
	}
}

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

// ProviderDefinition 向账号同步编排层暴露只读生命周期策略，不泄露具体 Adapter。
func (s *Service) ProviderDefinition(value accountdomain.Provider) (provider.Definition, bool) {
	if s.providers == nil {
		return provider.Definition{}, false
	}
	return s.providers.Definition(value)
}

func (s *Service) buildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error) {
	if s.buildBotFlagCache == nil {
		return s.loadBuildBotFlaggedAccountIDs(ctx)
	}
	return s.buildBotFlagCache.Load(ctx, buildBotFlagCacheKey, s.now(), func() ([]uint64, error) {
		return s.loadBuildBotFlaggedAccountIDs(ctx)
	})
}

// ListBuildBotFlaggedAccountIDs returns Build account IDs whose access-token claims
// mark bot_flag_source/bfs as 1 or 2. Used by routing to optionally exclude them.
func (s *Service) ListBuildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error) {
	return s.buildBotFlaggedAccountIDs(ctx)
}

// UpdateExcludeBuildBotFlaggedFromScheduling hot-updates whether bot-risk Build
// accounts are treated as non-schedulable in account summary available counts.
func (s *Service) UpdateExcludeBuildBotFlaggedFromScheduling(value bool) {
	s.maintenance.setExcludeBuildBot(value)
}

func (s *Service) excludeBuildBotFlaggedFromSchedulingEnabled() bool {
	return s.maintenance.excludeBuildBotFlagged()
}

func (s *Service) loadBuildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error) {
	if indexed, ok := s.accounts.(buildBotFlagIndexRepository); ok {
		return indexed.ListBuildBotFlaggedAccountIDs(ctx)
	}
	const batchSize = 500
	result := make([]uint64, 0)
	var afterID uint64
	for {
		values, _, err := s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderBuild, afterID, batchSize)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			if s.credentialMetadata(value).BuildBotFlagged {
				result = append(result, value.ID)
			}
		}
		if len(values) < batchSize {
			return result, nil
		}
		afterID = values[len(values)-1].ID
	}
}

// RebuildBuildBotFlagIndex backfills persisted non-sensitive routing metadata
// before the gateway begins serving traffic. Subsequent imports and refreshes
// update the source atomically with the encrypted access token.
func (s *Service) RebuildBuildBotFlagIndex(ctx context.Context) error {
	indexed, ok := s.accounts.(buildBotFlagIndexRepository)
	if !ok {
		return nil
	}
	const batchSize = 500
	var afterID uint64
	for {
		values, err := indexed.ListBuildBotFlagCredentialBatch(ctx, afterID, batchSize)
		if err != nil {
			return err
		}
		updates := make([]repository.BuildBotFlagSourceUpdate, 0)
		for _, value := range values {
			credential := accountdomain.Credential{
				ID: value.AccountID, Provider: accountdomain.ProviderBuild, EncryptedAccessToken: value.EncryptedAccessToken,
			}
			metadata := s.credentialMetadata(credential)
			if !metadata.BuildBotFlagInspected {
				continue
			}
			source := metadata.BuildBotFlagSource
			if source != 1 && source != 2 {
				source = 0
			}
			if source != value.StoredSource {
				updates = append(updates, repository.BuildBotFlagSourceUpdate{
					AccountID: value.AccountID, ExpectedEncryptedAccessToken: value.EncryptedAccessToken, Source: source,
				})
			}
		}
		if err := indexed.UpdateBuildBotFlagSources(ctx, updates); err != nil {
			return err
		}
		if len(values) < batchSize {
			s.invalidateBuildBotFlagCache()
			return nil
		}
		afterID = values[len(values)-1].AccountID
	}
}

func (s *Service) invalidateBuildBotFlagCache() {
	if s.buildBotFlagCache != nil {
		s.buildBotFlagCache.Delete(buildBotFlagCacheKey)
	}
}

// validAssociationFilter validates association filters against the selected provider.
// Web keeps its six Build/Console/combined values; Build and Console filter only by Web links.
func validAssociationFilter(providerValue, association string) bool {
	if association == "" {
		return true
	}
	switch providerValue {
	case string(accountdomain.ProviderWeb):
		return slices.Contains([]string{"buildLinked", "buildUnlinked", "consoleLinked", "consoleUnlinked", "allLinked", "allUnlinked"}, association)
	case string(accountdomain.ProviderBuild), string(accountdomain.ProviderConsole):
		return slices.Contains([]string{"webLinked", "webUnlinked"}, association)
	default:
		return false
	}
}

func (s *Service) Get(ctx context.Context, id uint64) (View, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	metadata := s.buildBotFlagMetadata(value)
	view := View{Credential: value, BuildBotFlagged: metadata.BuildBotFlagged, BuildBotFlagSource: metadata.BuildBotFlagSource}
	if billing, err := s.accounts.GetBilling(ctx, id); err == nil {
		view.Billing = &billing
	} else if !errors.Is(err, repository.ErrNotFound) {
		return View{}, err
	}
	observedTokens, err := s.audits.SumTokensByAccountsSince(ctx, []uint64{id}, s.now().Add(-freeUsageWindow))
	if err != nil {
		return View{}, err
	}
	var recovery *accountdomain.QuotaRecovery
	if recoveryValue, err := s.accounts.GetQuotaRecovery(ctx, id); err == nil {
		recovery = &recoveryValue
	} else if !errors.Is(err, repository.ErrNotFound) {
		return View{}, err
	}
	view.Quota = newQuotaView(view.Billing, observedTokens[id], recovery, value.ObservedModel, value.BuildSuperEntitled && value.Provider == accountdomain.ProviderBuild)
	if windows, err := s.accounts.GetQuotaWindows(ctx, []uint64{id}); err == nil {
		view.QuotaWindows = windows[id]
	} else {
		return View{}, err
	}
	states, err := s.readQualityStates(ctx)
	if err != nil {
		return View{}, err
	}
	view.Quality = qualityState(states, id)
	return view, nil
}

func (s *Service) credentialMetadata(value accountdomain.Credential) provider.CredentialMetadata {
	if s.providers == nil {
		return provider.CredentialMetadata{}
	}
	return s.providers.CredentialMetadata(value)
}

func (s *Service) buildBotFlagMetadata(value accountdomain.Credential) provider.CredentialMetadata {
	metadata := s.credentialMetadata(value)
	if metadata.BuildBotFlagInspected {
		return metadata
	}
	source := value.BuildBotFlagSource
	if source != 1 && source != 2 {
		source = 0
	}
	metadata.BuildBotFlagSource = source
	metadata.BuildBotFlagged = source != 0
	return metadata
}

func (s *Service) ObserveResponseModel(ctx context.Context, id uint64, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	_, err := s.observedModelWrites.Do(ctx, strconv.FormatUint(id, 10)+"\x00"+model, func() (any, error) {
		now := s.now()
		shard := s.observedModelShard(id)
		shard.Lock()
		if shard.values == nil {
			shard.values = make(map[uint64]observedModelState)
		}
		if shard.lastCleanupAt.IsZero() || now.Sub(shard.lastCleanupAt) >= observedModelPersistInterval {
			for accountID, state := range shard.values {
				if now.Sub(state.persistedAt) >= observedModelPersistInterval {
					delete(shard.values, accountID)
				}
			}
			shard.lastCleanupAt = now
		}
		state, exists := shard.values[id]
		localFresh := exists && state.model == model && observedModelStateIsFresh(now, state.persistedAt)
		if localFresh && (s.observedModelStore == nil || now.Sub(state.persistedAt) < observedModelLocalCacheTTL) {
			shard.Unlock()
			return nil, nil
		}
		shard.Unlock()
		if s.observedModelStore != nil {
			shared, ok, sharedErr := s.observedModelStore.GetObservedModelState(ctx, id)
			if sharedErr == nil && ok && shared.Model == model && observedModelStateIsFresh(now, shared.ObservedAt) {
				shard.Lock()
				shard.values[id] = observedModelState{model: model, persistedAt: now}
				shard.Unlock()
				return nil, nil
			}
		}
		updated := true
		if writer, ok := s.accounts.(repository.ObservedModelWriter); ok {
			var err error
			updated, err = writer.UpdateObservedModelIfNewer(ctx, id, model, now)
			if err != nil {
				return nil, err
			}
		} else if err := s.accounts.UpdateObservedModel(ctx, id, model, now); err != nil {
			return nil, err
		}
		if updated && s.observedModelStore != nil {
			_ = s.observedModelStore.SetObservedModelState(ctx, id, repository.ObservedModelState{Model: model, ObservedAt: now}, observedModelPersistInterval)
		}
		shard.Lock()
		current, exists := shard.values[id]
		if !exists || !current.persistedAt.After(now) {
			shard.values[id] = observedModelState{model: model, persistedAt: now}
		}
		shard.Unlock()
		return nil, nil
	})
	return err
}

func (s *Service) observedModelShard(id uint64) *observedModelShard {
	return &s.observedModelShards[id%observedModelLockShards]
}

func observedModelStateIsFresh(now, persistedAt time.Time) bool {
	elapsed := now.Sub(persistedAt)
	return elapsed >= 0 && elapsed < observedModelPersistInterval
}

func newQuotaView(billing *accountdomain.Billing, observedTokens int64, recovery *accountdomain.QuotaRecovery, observedModel string, buildSuperEntitled bool) QuotaView {
	// Upstream paid billing takes precedence and preserves reported quota values.
	if billing != nil && billing.IsPaid() {
		periodStart, periodEnd := billing.BillingPeriodStart, billing.BillingPeriodEnd
		if billing.UsagePeriodType != "" {
			periodStart, periodEnd = billing.UsagePeriodStart, billing.UsagePeriodEnd
		}
		result := QuotaView{Type: QuotaTypePaid, Source: "upstreamBilling", Confidence: "observed", Unit: "credits", UsagePercent: billing.CreditUsagePercent, Status: QuotaStatusActive, PeriodStart: periodStart, PeriodEnd: periodEnd}
		if recovery != nil && recovery.Kind == accountdomain.QuotaRecoveryKindPaid {
			result.Status = QuotaStatusWaitingReset
			if recovery.Status == accountdomain.QuotaRecoveryStatusProbing {
				result.Status = QuotaStatusProbing
			}
			result.ExhaustedAt = recovery.ExhaustedAt
			result.NextProbeAt = recovery.NextProbeAt
			result.LastConfirmedAt = recovery.LastConfirmedAt
		}
		switch {
		case billing.MonthlyLimit > 0:
			result.Used = billing.Used
			result.Limit = billing.MonthlyLimit
			result.Remaining = billing.Remaining()
			result.UsagePercent = billing.Used / billing.MonthlyLimit * 100
			result.LimitKnown = true
		case billing.OnDemandCap > 0:
			result.Limit = billing.OnDemandCap
			result.Used = billing.OnDemandUsed
			if result.Used == 0 && billing.CreditUsagePercent > 0 {
				result.Used = billing.OnDemandCap * billing.CreditUsagePercent / 100
			}
			result.Remaining = billing.OnDemandCap - result.Used
			result.LimitKnown = true
			if result.Remaining < 0 {
				result.Remaining = 0
			}
		case billing.PrepaidBalance > 0:
			result.Remaining = billing.PrepaidBalance
		case billing.UsagePeriodType != "":
			result.Unit = "percent"
			result.Used = billing.CreditUsagePercent
			result.Limit = 100
			result.Remaining = max(0, 100-billing.CreditUsagePercent)
			result.LimitKnown = true
		}
		return result
	}
	// 管理员确认的 Build Super entitlement：覆盖 Free recovery / profile / observed free 等弱信号。
	// 不伪造额度、余额、使用率或账期；Billing 数值保持未知/零。
	if buildSuperEntitled {
		return QuotaView{
			Type: QuotaTypePaid, Source: "buildSuperEntitlement", Confidence: "confirmed",
			Confirmed: true, Status: QuotaStatusActive,
		}
	}
	if recovery != nil && recovery.Status != accountdomain.QuotaRecoveryStatusActive && (recovery.Kind == "" || recovery.Kind == accountdomain.QuotaRecoveryKindFree) {
		limit := recovery.ConfirmedLimit
		used := recovery.ConfirmedUsed
		if used <= 0 {
			used = observedTokens
		}
		status := QuotaStatusWaitingReset
		if recovery.Status == accountdomain.QuotaRecoveryStatusProbing {
			status = QuotaStatusProbing
		}
		remaining := int64(0)
		usagePercent := 0.0
		if limit > 0 {
			remaining = limit - used
			if remaining < 0 {
				remaining = 0
			}
			usagePercent = float64(used) / float64(limit) * 100
		}
		return QuotaView{
			Type: QuotaTypeFree, Source: "upstreamExhaustion", Confidence: "confirmed", Unit: "tokens", Used: float64(used), Limit: float64(limit), LimitKnown: limit > 0,
			Remaining: float64(remaining), UsagePercent: usagePercent,
			WindowHours: int(freeUsageWindow / time.Hour), Confirmed: true, Status: status,
			ExhaustedAt: recovery.ExhaustedAt, NextProbeAt: recovery.NextProbeAt, LastConfirmedAt: recovery.LastConfirmedAt,
		}
	}
	freeSource := ""
	confidence := ""
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(observedModel)), "-build-free") {
		freeSource = "responseModel"
		confidence = "observed"
	} else if isEstimatedFreeBillingProfile(billing) {
		freeSource = "billingProfile"
		confidence = "estimated"
	}
	if freeSource == "" {
		return QuotaView{Type: QuotaTypeUnknown, Source: "unknown", Status: QuotaStatusActive}
	}
	if observedTokens < 0 {
		observedTokens = 0
	}
	remaining := estimatedFreeTokenLimit - observedTokens
	if remaining < 0 {
		remaining = 0
	}
	return QuotaView{
		Type:         QuotaTypeFree,
		Source:       freeSource,
		Confidence:   confidence,
		Unit:         "tokens",
		Used:         float64(observedTokens),
		Limit:        float64(estimatedFreeTokenLimit),
		Remaining:    float64(remaining),
		UsagePercent: float64(observedTokens) / float64(estimatedFreeTokenLimit) * 100,
		LimitKnown:   false,
		WindowHours:  int(freeUsageWindow / time.Hour),
		Observed:     true,
		Status:       QuotaStatusActive,
	}
}

func isEstimatedFreeBillingProfile(billing *accountdomain.Billing) bool {
	return billing != nil && (billing.HasFreeProfileSignal() || billing.HasInferredFreeProfileSignal())
}

// ClearCooldown applies an explicit administrator command against current health.
func (s *Service) ClearCooldown(ctx context.Context, id uint64) (View, error) {
	if _, err := s.accounts.ApplyHealth(ctx, id, "", accountdomain.HealthEvent{Kind: accountdomain.HealthClearCooldown}); err != nil {
		return View{}, mapRepositoryError(err)
	}
	return s.Get(ctx, id)
}

// ClearQualityHold lifts the quality marker an account carries together with the
// transient cooldown that came with it. The quality court calls this once a
// verdict has exonerated the account, so a cleared account does not have to wait
// out a cooldown that was only ever a symptom of the accusation.
//
// It applies to quality markers only (missing_thinking, quality_idle_timeout —
// see NormalizeHealthMarker). An account whose current restriction is a plain
// transport fact such as an upstream 429 keeps it: that fact belongs to the
// account axis, and an exit verdict must not erase it. MinHold stays zero so a
// release takes effect immediately.
func (s *Service) ClearQualityHold(ctx context.Context, id uint64) error {
	if id == 0 {
		return nil
	}
	if _, err := s.accounts.ApplyHealth(ctx, id, "", accountdomain.HealthEvent{Kind: accountdomain.HealthClearQuality}); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

// MarkBuildAPIFallback 幂等写入 Build 账号 XAI 推理回退标记；失败不吞掉，调用方可重试。
func (s *Service) MarkBuildAPIFallback(ctx context.Context, id uint64, enabled bool) error {
	return mapRepositoryError(s.accounts.MarkBuildAPIFallback(ctx, id, enabled))
}

func (s *Service) MarkReauthRequired(ctx context.Context, observed accountdomain.CredentialRef, reason string) error {
	_, err := s.applyCredentialRejection(ctx, observed, reason)
	return err
}

// applyCredentialRejection preserves the committed result for consumers that
// report a state transition. Ordinary request callers may ignore stale events.
func (s *Service) applyCredentialRejection(ctx context.Context, observed accountdomain.CredentialRef, reason string) (accountdomain.CredentialResult, error) {
	result, err := s.accounts.ApplyCredential(ctx, observed, accountdomain.CredentialEvent{Kind: accountdomain.CredentialRejected, Reason: reason, OccurredAt: s.now()})
	if err != nil {
		return result, mapRepositoryError(err)
	}
	if result.Applied && result.Credential.CredentialGeneration == observed.Generation && s.sticky != nil {
		_ = s.sticky.DeleteByAccount(ctx, observed.AccountID)
	}
	return result, nil
}

// markSSOCredentialRejected 在上游明确返回 401 后可靠持久化失效状态。
// 状态写入不继承客户端取消，避免已经确认失效的账号因请求断开继续留在号池。
func (s *Service) markSSOCredentialRejected(ctx context.Context, value accountdomain.Credential, reason string) error {
	if value.AuthType != accountdomain.AuthTypeSSO {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	if err := s.MarkReauthRequired(writeCtx, value.CredentialRef(), reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "account_id", value.ID, "provider", value.Provider, "error", err)
		return err
	}
	return nil
}

// EnsureCredential 在即将过期时刷新 token，同一账号并发请求只执行一次刷新。
func (s *Service) EnsureCredential(ctx context.Context, value accountdomain.Credential, force bool) (accountdomain.Credential, error) {
	return s.ensureCredential(ctx, value, ensureCredentialOptions{force: force})
}

type ensureCredentialOptions struct {
	force              bool
	bypassCooldown     bool
	respectSchedule    bool
	retryPermanentOnce bool
}

func (s *Service) ensureCredential(ctx context.Context, value accountdomain.Credential, options ensureCredentialOptions) (accountdomain.Credential, error) {
	if s.providers == nil || !s.providers.SupportsCredentialRefresh(value.Provider) {
		if options.force {
			return accountdomain.Credential{}, ErrUnsupported
		}
		return value, nil
	}
	now := s.now()
	if credential, err, handled := s.resolvePermanentRefreshFailure(ctx, value, now, options.force, options.retryPermanentOnce); handled {
		return credential, err
	}
	if !options.force && value.ExpiresAt.IsZero() && value.EncryptedAccessToken != "" {
		return value, nil
	}
	if !options.force && value.EncryptedAccessToken != "" && !value.ExpiresAt.IsZero() && now.Add(credentialRefreshAdvance).Before(value.ExpiresAt) {
		return value, nil
	}
	refreshKey := strconv.FormatUint(value.ID, 10) + ":" + strconv.FormatUint(value.CredentialGeneration, 10)
	if options.respectSchedule {
		refreshKey += ":scheduled"
	}
	if options.retryPermanentOnce {
		refreshKey += ":manual-retry"
	}
	result, err := s.credentialLifecycle.group.Do(ctx, refreshKey, func() (any, error) {
		latest, err := s.accounts.Get(ctx, value.ID)
		if err != nil {
			return nil, err
		}
		currentTime := s.now()
		if credential, err, handled := s.resolvePermanentRefreshFailure(ctx, latest, currentTime, options.force, options.retryPermanentOnce); handled {
			if err != nil {
				return nil, err
			}
			return credential, nil
		}
		if options.respectSchedule && latest.RefreshDueAt != nil && latest.RefreshDueAt.After(currentTime) {
			return latest, nil
		}
		if options.force && latest.EncryptedAccessToken != "" && latest.CredentialGeneration != value.CredentialGeneration {
			return latest, nil
		}
		if !options.force && latest.EncryptedAccessToken != "" && !latest.ExpiresAt.IsZero() && currentTime.Add(credentialRefreshAdvance).Before(latest.ExpiresAt) {
			return latest, nil
		}
		if options.force && !options.bypassCooldown && s.credentialRefreshCoolingDown(latest, currentTime) {
			return latest, nil
		}
		release, err := s.acquireRefreshLock(ctx, latest.ID)
		if err != nil {
			return nil, err
		}
		if release != nil {
			defer release()
			latest, err = s.accounts.Get(ctx, value.ID)
			if err != nil {
				return nil, err
			}
			currentTime = s.now()
			if credential, err, handled := s.resolvePermanentRefreshFailure(ctx, latest, currentTime, options.force, options.retryPermanentOnce); handled {
				if err != nil {
					return nil, err
				}
				return credential, nil
			}
			if options.respectSchedule && latest.RefreshDueAt != nil && latest.RefreshDueAt.After(currentTime) {
				return latest, nil
			}
			if options.force && !options.bypassCooldown && s.credentialRefreshCoolingDown(latest, currentTime) {
				return latest, nil
			}
			if latest.EncryptedAccessToken != "" && latest.CredentialGeneration != value.CredentialGeneration {
				return latest, nil
			}
			if !options.force && latest.EncryptedAccessToken != "" && !latest.ExpiresAt.IsZero() && currentTime.Add(credentialRefreshAdvance).Before(latest.ExpiresAt) {
				return latest, nil
			}
		}
		adapter, ok := s.providers.CredentialRefresh(latest.Provider)
		if !ok {
			return nil, fmt.Errorf("Provider %s 未注册", latest.Provider)
		}
		refreshed, err := adapter.RefreshCredential(ctx, latest)
		if err != nil {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialRefreshStateTTL)
			s.recordCredentialRefreshFailure(persistCtx, latest, err, !options.retryPermanentOnce, release != nil)
			cancel()
			return nil, err
		}
		riskCredential := latest
		riskCredential.EncryptedAccessToken = refreshed.EncryptedAccessToken
		botFlagSource := latest.BuildBotFlagSource
		if metadata := s.credentialMetadata(riskCredential); metadata.BuildBotFlagInspected {
			botFlagSource = metadata.BuildBotFlagSource
		}
		persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
		result, err := s.accounts.ApplyCredential(persistCtx, latest.CredentialRef(), accountdomain.CredentialEvent{Kind: accountdomain.CredentialRefreshed, AccessToken: refreshed.EncryptedAccessToken, RefreshToken: refreshed.EncryptedRefreshToken, ExpiresAt: refreshed.ExpiresAt, BuildBotFlagSource: botFlagSource, OccurredAt: s.now()})
		cancelPersist()
		if err != nil {
			s.logger.Error("credential_refresh_token_write_failed",
				"account_id", latest.ID,
				"provider", latest.Provider,
				"refresh_token_rotated", refreshed.RefreshTokenRotated,
				"build_api_fallback_marked", latest.BuildAPIFallback,
				"distributed_lock", release != nil,
				"error", err,
			)
			return nil, err
		}
		s.invalidateBuildBotFlagCache()
		s.WakeCredentialRefresh()
		return result.Credential, nil
	})
	if err != nil {
		return accountdomain.Credential{}, err
	}
	credential, ok := result.(accountdomain.Credential)
	if !ok {
		return accountdomain.Credential{}, fmt.Errorf("账号凭据刷新返回类型无效")
	}
	return credential, nil
}

// acquireRefreshLock 在 Redis 模式下等待其他实例完成刷新，锁租约过期后可自动接管。
func (s *Service) acquireRefreshLock(ctx context.Context, accountID uint64) (func(), error) {
	if s.refreshLock == nil {
		return nil, nil
	}
	key := "credential-refresh:" + strconv.FormatUint(accountID, 10)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		release, acquired, err := s.refreshLock.Acquire(ctx, key, 2*time.Minute)
		if err != nil {
			return nil, err
		}
		if acquired {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) RefreshToken(ctx context.Context, id uint64) (View, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	if _, err := s.ensureCredential(ctx, value, ensureCredentialOptions{force: true, bypassCooldown: true, retryPermanentOnce: true}); err != nil {
		return View{}, err
	}
	return s.Get(ctx, id)
}

func (s *Service) credentialRefreshCoolingDown(credential accountdomain.Credential, now time.Time) bool {
	if credential.LastRefreshAt == nil {
		return false
	}
	age := now.Sub(*credential.LastRefreshAt)
	return age >= 0 && age < forcedRefreshMinInterval
}

func (s *Service) recordCredentialRefreshFailure(ctx context.Context, credential accountdomain.Credential, refreshErr error, preservePermanent, distributedLock bool) {
	if neterror.IsLocalExecution(refreshErr) || errors.Is(refreshErr, context.Canceled) || errors.Is(refreshErr, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	errorCode := "oauth_transport_error"
	errorMessage := "OAuth request failed"
	errorStatus := 0
	errorResponse := ""
	permanent := false
	retryAfter := time.Duration(0)
	var typed *provider.CredentialRefreshError
	if errors.As(refreshErr, &typed) {
		errorCode = strings.TrimSpace(typed.Code)
		if errorCode == "" {
			errorCode = "oauth_refresh_error"
		}
		errorStatus = typed.Status
		permanent = typed.Permanent
		retryAfter = typed.RetryAfter
		if message := normalizeCredentialRefreshErrorMessage(typed.Message); message != "" {
			errorMessage = message
		}
		errorResponse = normalizeCredentialRefreshErrorResponse(typed.Response)
	} else if errors.Is(refreshErr, context.DeadlineExceeded) {
		errorCode = "oauth_timeout"
		errorMessage = "OAuth request timed out"
	}

	result, err := s.accounts.ApplyCredential(ctx, credential.CredentialRef(), accountdomain.CredentialEvent{Kind: accountdomain.CredentialRefreshFailed, OccurredAt: s.now(), Failure: accountdomain.CredentialRefreshFailure{
		Status: errorStatus, Code: errorCode, Message: errorMessage, Response: errorResponse, Permanent: permanent, PreservePermanent: preservePermanent, RetryAfter: retryAfter,
	}})
	if err != nil {
		s.logger.Warn("credential_refresh_state_write_failed", "account_id", credential.ID, "error", err)
		return
	}
	if !result.Applied {
		return
	}
	current := result.Credential
	s.logger.Warn("credential_refresh_failed", "account_id", credential.ID, "provider", credential.Provider,
		"http_status", errorStatus, "error_code", errorCode, "error_message", errorMessage,
		"permanent", current.RefreshPermanent, "failure_count", current.RefreshFailureCount,
		"unclassified_auth_failure_count", current.RefreshUnclassifiedAuthCount,
		"requires_reauth", current.AuthStatus == accountdomain.AuthStatusReauthRequired, "retry_at", current.RefreshDueAt,
		"refresh_token_rotated", false, "distributed_lock", distributedLock)
	if current.AuthStatus == accountdomain.AuthStatusReauthRequired && s.sticky != nil {
		_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	}
	s.WakeCredentialRefresh()
}

func normalizeCredentialRefreshErrorMessage(value string) string {
	return accountdomain.NormalizeCredentialRefreshErrorMessage(value)
}

func normalizeCredentialRefreshErrorResponse(value string) string {
	return accountdomain.NormalizeCredentialRefreshErrorResponse(value)
}

// resolvePermanentRefreshFailure 阻止自动链路再次请求已确认失效的 refresh token，
// 并在 access token 到期后收敛账号状态。管理员显式刷新可通过
// retryPermanentOnce 绕过一次；credential_decrypt_failed 等可恢复本地错误不受阻断。
func (s *Service) resolvePermanentRefreshFailure(ctx context.Context, credential accountdomain.Credential, now time.Time, force, retryPermanentOnce bool) (accountdomain.Credential, error, bool) {
	if !credential.RefreshPermanent {
		return accountdomain.Credential{}, nil, false
	}
	if isRecoverableRefreshErrorCode(credential.LastRefreshErrorCode) {
		// 允许 force 或到期调度再次尝试解密/刷新；成功后会 clear permanent 标记。
		return accountdomain.Credential{}, nil, false
	}
	if retryPermanentOnce {
		return accountdomain.Credential{}, nil, false
	}
	accessTokenAlive := credential.EncryptedAccessToken != "" && !credential.ExpiresAt.IsZero() && credential.ExpiresAt.After(now)
	if accessTokenAlive && !force {
		return credential, nil, true
	}
	if !accessTokenAlive {
		if err := s.MarkReauthRequired(ctx, credential.CredentialRef(), permanentRefreshExpiredReason); err != nil {
			return accountdomain.Credential{}, err, true
		}
	}
	if credential.LastRefreshErrorCode == "" {
		return accountdomain.Credential{}, ErrCredentialRefreshPermanent, true
	}
	return accountdomain.Credential{}, fmt.Errorf("%w: %s", ErrCredentialRefreshPermanent, credential.LastRefreshErrorCode), true
}

// isRecoverableRefreshErrorCode 标识“永久标记可被后续成功刷新清除”的本地/临时错误。
func isRecoverableRefreshErrorCode(code string) bool {
	return accountdomain.IsRecoverableCredentialRefreshErrorCode(code)
}

// RefreshAllTokensWithProgress 续期所有声明支持刷新的 Provider 凭据，不可续期账号会被跳过。
func (s *Service) RefreshAllTokensWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, int, error) {
	if s.providers == nil {
		return 0, 0, 0, fmt.Errorf("Provider 注册表未初始化")
	}
	allIDs := make([]uint64, 0)
	ids := make([]uint64, 0)
	for _, providerValue := range s.providers.Providers() {
		if !s.providers.SupportsCredentialRefresh(providerValue) {
			continue
		}
		providerIDs, err := s.accounts.ListEnabledCredentialRefreshAccountIDs(ctx, providerValue, false)
		if err != nil {
			return 0, 0, 0, err
		}
		refreshableIDs, err := s.accounts.ListEnabledCredentialRefreshAccountIDs(ctx, providerValue, true)
		if err != nil {
			return 0, 0, 0, err
		}
		allIDs = append(allIDs, providerIDs...)
		ids = append(ids, refreshableIDs...)
	}
	skipped := max(0, len(allIDs)-len(ids))
	succeeded, failed, err := s.refreshTokens(ctx, ids, progress)
	return succeeded, failed, skipped, err
}

func (s *Service) refreshTokens(ctx context.Context, ids []uint64, progress BatchProgressObserver) (int, int, error) {
	return s.runAccountBatch(ctx, "credential_refresh", ids, s.refreshPool, progress, func(workCtx context.Context, id uint64) error {
		value, err := s.accounts.Get(workCtx, id)
		if err == nil {
			_, err = s.ensureCredential(workCtx, value, ensureCredentialOptions{force: true, bypassCooldown: true, retryPermanentOnce: true})
		}
		return err
	})
}

// BatchRefreshTokens 续期指定账号的凭据；失效账号会强制向上游重试一次，
// 停用、Provider 不支持或缺少刷新凭据的账号会被跳过。
func (s *Service) BatchRefreshTokens(ctx context.Context, ids []uint64) (int, int, int, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, 0, 0, err
	}
	if s.providers == nil {
		return 0, 0, 0, fmt.Errorf("Provider 注册表未初始化")
	}
	refreshableIDs := make([]uint64, 0, len(values))
	for _, id := range values {
		value, getErr := s.accounts.Get(ctx, id)
		if getErr != nil {
			// 管理端并发删除下 ID 消失是常态:与 BatchDelete 的容错语义对齐,
			// 跳过而非中止整批(一个消失的 ID 不该让其他健康账号都不续期)。
			if errors.Is(getErr, repository.ErrNotFound) {
				continue
			}
			return 0, 0, 0, getErr
		}
		if !s.providers.SupportsCredentialRefresh(value.Provider) || !value.Enabled || value.EncryptedRefreshToken == "" {
			continue
		}
		refreshableIDs = append(refreshableIDs, id)
	}
	// 预检消失 + 不可续期的账号都计入 skipped。
	skipped := len(values) - len(refreshableIDs)
	succeeded, failed, err := s.refreshTokens(ctx, refreshableIDs, nil)
	return succeeded, failed, skipped, err
}

func (s *Service) runAccountBatch(ctx context.Context, operation string, ids []uint64, pool *batch.Pool, progress BatchProgressObserver, work func(context.Context, uint64) error) (int, int, error) {
	if progress != nil {
		if err := progress(0, len(ids)); err != nil {
			return 0, 0, err
		}
	}
	var progressMu sync.Mutex
	var progressErr error
	completed := 0
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results, summary, err := batch.MapObserved(runCtx, ids, batch.Options{Workers: pool.Limit(), Pool: pool}, func(workCtx context.Context, id uint64) (struct{}, error) {
		return struct{}{}, work(workCtx, id)
	}, func(_ int, _ batch.Result[struct{}]) {
		progressMu.Lock()
		defer progressMu.Unlock()
		completed++
		if progress != nil {
			if notifyErr := progress(completed, len(ids)); notifyErr != nil && progressErr == nil {
				progressErr = notifyErr
				cancel()
			}
		}
	})
	for index, result := range results {
		var panicErr *batch.PanicError
		if errors.As(result.Err, &panicErr) {
			s.logger.Error("account_bulk_task_panicked", "operation", operation, "account_id", ids[index], "error", panicErr, "stack", string(panicErr.Stack))
		}
	}
	s.logBatchSummary(operation, pool, summary, err)
	return summary.Succeeded, summary.Failed, errors.Join(err, progressErr)
}

func (s *Service) logBatchSummary(operation string, pool *batch.Pool, summary batch.Summary, err error) {
	snapshot := pool.Snapshot()
	s.logger.Info("account_bulk_completed", "operation", operation, "total", summary.Total, "submitted", summary.Submitted, "succeeded", summary.Succeeded, "failed", summary.Failed, "panicked", summary.Panicked, "duration_ms", summary.Duration.Milliseconds(), "canceled", summary.Canceled, "pool_limit", snapshot.Limit, "pool_active", snapshot.Active, "pool_queued", snapshot.Queued, "pool_peak", snapshot.Peak, "error", err)
}

func (s *Service) persistSeed(ctx context.Context, seed provider.CredentialSeed, source, target *accountdomain.CredentialRef) (repository.AccountUpsertResult, error) {
	value, err := s.credentialFromSeed(seed)
	if err != nil {
		return repository.AccountUpsertResult{}, err
	}
	results, err := s.accounts.ImportAccounts(ctx, []repository.AccountImport{{Credential: value, Source: source, Target: target}})
	if err != nil {
		return repository.AccountUpsertResult{}, mapRepositoryError(err)
	}
	if len(results) != 1 {
		return repository.AccountUpsertResult{}, fmt.Errorf("导入账号持久化结果数量无效: %d", len(results))
	}
	result := results[0]
	if result.Skipped != "" {
		return result, nil
	}
	if result.ID == 0 || result.Material.AccountID != result.ID || result.Material.Provider != value.Provider {
		return repository.AccountUpsertResult{}, fmt.Errorf("导入账号持久化材料引用无效")
	}
	s.invalidateBuildBotFlagCache()
	s.WakeCredentialRefresh()
	return result, nil
}

func (s *Service) credentialFromSeed(seed provider.CredentialSeed) (accountdomain.Credential, error) {
	accessEncrypted, err := s.cipher.Encrypt(seed.AccessToken)
	if err != nil {
		return accountdomain.Credential{}, err
	}
	refreshEncrypted, err := s.cipher.Encrypt(seed.RefreshToken)
	if err != nil {
		return accountdomain.Credential{}, err
	}
	cloudflareEncrypted := ""
	if strings.TrimSpace(seed.CloudflareCookies) != "" {
		cookies := cfcookies.Sanitize(seed.CloudflareCookies)
		if cookies == "" {
			return accountdomain.Credential{}, invalidInput("Cloudflare Cookie 中没有有效字段")
		}
		cloudflareEncrypted, err = s.cipher.Encrypt(cookies)
		if err != nil {
			return accountdomain.Credential{}, err
		}
	}
	sourceKey := seed.SourceKey
	if sourceKey == "" {
		sourceKey = "device:" + tokenhash.HashToken(seed.AccessToken)
	}
	providerValue := seed.Provider
	if providerValue == "" {
		providerValue = accountdomain.ProviderBuild
	}
	authType := seed.AuthType
	if authType == "" {
		if s.providers == nil {
			return accountdomain.Credential{}, fmt.Errorf("Provider 注册表未初始化")
		}
		definition, ok := s.providers.Definition(providerValue)
		if !ok {
			return accountdomain.Credential{}, fmt.Errorf("Provider %s 未注册", providerValue)
		}
		authType = definition.Credential.AuthType
	}
	value := accountdomain.Credential{Provider: providerValue, AuthType: authType, WebTier: seed.WebTier, Name: seed.Name, Email: seed.Email, UserID: seed.UserID, TeamID: seed.TeamID, SourceKey: sourceKey, OIDCClientID: seed.OIDCClientID, EncryptedAccessToken: accessEncrypted, EncryptedRefreshToken: refreshEncrypted, EncryptedCloudflareCookie: cloudflareEncrypted, ExpiresAt: seed.ExpiresAt, Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: accountdomain.DefaultPriority, MaxConcurrent: accountdomain.DefaultMaxConcurrent, MinimumRemaining: accountdomain.DefaultMinimumRemaining, WebNSFWEnabledAt: seed.WebNSFWEnabledAt, WebTermsAcceptedAt: seed.WebTermsAcceptedAt, WebTermsAcceptedVersion: seed.WebTermsAcceptedVersion, WebBirthDateSetAt: seed.WebBirthDateSetAt}
	value.BuildBotFlagSource = s.credentialMetadata(value).BuildBotFlagSource
	if providerValue == accountdomain.ProviderWeb && strings.TrimSpace(seed.AccessToken) != "" {
		value.EgressIdentity = "sso_" + tokenhash.HashToken(seed.AccessToken)[:32]
	}
	return value, nil
}

func normalizePage(page, pageSize int) (int, int) {
	return repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
}

func normalizeBatchIDs(ids []uint64) ([]uint64, error) {
	return normalizeIDs(ids, repository.MaxPageSize)
}

func normalizeIDs(ids []uint64, limit int) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, invalidInput("至少选择一个账号")
	}
	if len(ids) > limit {
		return nil, invalidInput(fmt.Sprintf("单次最多处理 %d 个账号", limit))
	}
	seen := make(map[uint64]struct{}, len(ids))
	result := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, invalidInput("账号 ID 无效")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}

// invalidInput 为可安全返回给管理端的账号参数错误附加稳定语义。
func invalidInput(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, message)
}

// mapRepositoryError 隔离持久化层错误，避免 transport 依赖仓储实现语义。
func mapLinkedDeleteError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "关联删除目标") || strings.Contains(msg, "账号来源无效") || strings.Contains(msg, "不支持清理账号状态") {
		return invalidInput(msg)
	}
	return mapRepositoryError(err)
}

func mapRepositoryError(err error) error {
	if errors.Is(err, repository.ErrAccountPoolMismatch) {
		return ErrAccountPoolMismatch
	}
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, repository.ErrConflict) {
		return fmt.Errorf("%w: %s", ErrConflict, strings.TrimPrefix(err.Error(), repository.ErrConflict.Error()+": "))
	}
	return err
}
