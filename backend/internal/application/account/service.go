package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/cfcookies"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
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
	unknownRemoteQuotaProbeDelay    time.Duration = 5 * time.Minute
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

type quotaRefreshResult struct {
	Credential accountdomain.Credential
	Windows    []accountdomain.QuotaWindow
	Modes      []string
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

type UpdateInput struct {
	Name                   *string
	Enabled                *bool
	Priority               *int
	MaxConcurrent          *int
	MinimumRemaining       *float64
	CloudflareCookies      *string
	ClearCloudflareCookies bool
	// BuildSuperEntitled 仅 grok_build 可设置；非 Build 返回业务错误。
	BuildSuperEntitled *bool
	// BuildRouteMode 仅 grok_build 可设置；nil 表示不修改。
	BuildRouteMode *accountdomain.BuildRouteMode
	// RiskStatus 设置长期风险标记：仅允许 "" 与 "rsc_denied"。标记后账号
	// 保持 enabled，但调度跳过，直到人工清空或 DeniedTTL 后巡检复测 clean。
	RiskStatus *string
}

type CleanupStatus string

const (
	CleanupStatusCooldown       CleanupStatus = "cooldown"
	CleanupStatusDisabled       CleanupStatus = "disabled"
	CleanupStatusReauthRequired CleanupStatus = "reauthRequired"
)

type DeviceStartResult struct {
	SessionID               string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresAt               time.Time
}

type ImportResult struct {
	Created    int
	Updated    int
	Skipped    int
	Failed     int
	AccountIDs []uint64
}

type BuildConversionStrategy string

const (
	BuildConversionAll     BuildConversionStrategy = "all"
	BuildConversionMissing BuildConversionStrategy = "missing"
)

type WebConsoleSyncStrategy string

const (
	WebConsoleSyncAll     WebConsoleSyncStrategy = "all"
	WebConsoleSyncMissing WebConsoleSyncStrategy = "missing"
)

type ImportedAccountObserver func(accountID uint64) error

// BatchProgressObserver 在单个账号任务结束后报告批次完成数。
type BatchProgressObserver func(completed, total int) error

type ExportResult struct {
	Data  []byte
	Count int
}

type ExportPageResult struct {
	ExportResult
	NextID        uint64
	SnapshotMaxID uint64
	HasMore       bool
}

type BuildConversionResult struct {
	Created         int
	Linked          int
	Skipped         int
	Failed          int
	BuildAccountIDs []uint64
}

type ListFilter struct {
	Provider  string
	QuotaType string
	Status    string
	Renewal   string
	Risk      string
	// Agreement applies only to grok_web accounts.
	Agreement string
	// Association values are provider-specific: Web supports build, console, and combined filters;
	// Build and Console support only webLinked and webUnlinked.
	Association string
	Sort        repository.SortQuery
}

type Summary struct {
	Total      int64
	Available  int64
	Recovering int64
	Attention  int64
	Risk       int64
	Providers  map[string]ProviderSummary
	Recovery   RecoverySummary
	Issues     IssueSummary
}

type ProviderSummary struct {
	Total     int64
	Available int64
}

type RecoverySummary struct {
	Cooldown     int64
	WaitingReset int64
	Probing      int64
}

type IssueSummary struct {
	Disabled       int64
	ReauthRequired int64
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	now := s.now()
	rows, err := s.accounts.Summarize(ctx, now)
	if err != nil {
		return Summary{}, err
	}
	result := Summary{Providers: make(map[string]ProviderSummary, len(accountdomain.Providers()))}
	for _, providerValue := range accountdomain.Providers() {
		result.Providers[string(providerValue)] = ProviderSummary{}
	}
	riskFlagged := int64(0)
	for _, row := range rows {
		result.Total += row.Total
		result.Available += row.Available
		result.Recovery.Cooldown += row.Cooldown
		result.Recovery.WaitingReset += row.WaitingReset
		result.Recovery.Probing += row.Probing
		result.Issues.Disabled += row.Disabled
		result.Issues.ReauthRequired += row.ReauthRequired
		riskFlagged += row.RiskFlagged
		result.Providers[row.Provider] = ProviderSummary{Total: row.Total, Available: row.Available}
	}
	result.Recovering = result.Recovery.Cooldown + result.Recovery.WaitingReset + result.Recovery.Probing
	result.Attention = result.Issues.Disabled + result.Issues.ReauthRequired
	indexed, hasIndex := s.accounts.(buildBotFlagIndexRepository)
	var flaggedIDs []uint64
	if hasIndex {
		result.Risk, err = indexed.CountBuildBotFlagged(ctx)
	} else {
		flaggedIDs, err = s.buildBotFlaggedAccountIDs(ctx)
		result.Risk = int64(len(flaggedIDs))
	}
	// 长期风险标记（rsc_denied）与凭据级 bot flag 同为“风控”口径。
	result.Risk += riskFlagged
	if err != nil {
		return Summary{}, err
	}
	if s.excludeBuildBotFlaggedFromSchedulingEnabled() && result.Risk > 0 {
		var excluded int64
		if hasIndex {
			excluded, err = indexed.CountAvailableBuildBotFlagged(ctx, now)
		} else {
			excluded, err = s.accounts.CountAvailableAmong(ctx, accountdomain.ProviderBuild, flaggedIDs, now)
		}
		if err != nil {
			return Summary{}, err
		}
		if excluded > 0 {
			buildKey := string(accountdomain.ProviderBuild)
			build := result.Providers[buildKey]
			if excluded > build.Available {
				excluded = build.Available
			}
			build.Available -= excluded
			result.Providers[buildKey] = build
			if excluded > result.Available {
				excluded = result.Available
			}
			result.Available -= excluded
		}
	}
	return result, nil
}

// Service 负责 OAuth 账号接入、刷新、额度和持久化生命周期。
type Service struct {
	rateLimitMu         sync.Mutex
	rateLimitActive     atomic.Bool
	rateLimitNextExpiry atomic.Int64
	rateLimits          map[string]TeamModelRateLimit
	rateLimitTeams      map[teamRateLimitIdentity]teamRateLimitObservation
	accounts            repository.AccountRepository
	audits              repository.AuditRepository
	deviceSessions      repository.DeviceSessionRepository
	sticky              repository.StickySessionRepository
	refreshLock         repository.DistributedLock
	concurrency         repository.ConcurrencyLimiter
	quotaQueue          repository.QuotaRecoveryQueue
	quotaRefreshState   repository.QuotaRefreshCoordinator
	providers           *provider.Registry
	cipher              security.Cryptor
	refreshes           OperationGroup[string]
	billingSyncs        OperationGroup[string]
	quotaSyncs          OperationGroup[string]
	identitySyncs       OperationGroup[accountdomain.CredentialRef]
	observedModelWrites OperationGroup[string]
	observedModelStore  repository.ObservedModelStateRepository
	observedModelShards [observedModelLockShards]observedModelShard
	quotaRefreshMu      sync.Mutex
	quotaRefreshes      map[string]*quotaRefreshState
	quotaRefreshQueue   chan quotaRefreshRequest
	quotaRefreshWake    chan struct{}
	quotaRefreshCursor  uint64
	quotaDurableScan    uint64
	conversionPool      *batch.Pool
	syncPool            *batch.Pool
	refreshPool         *batch.Pool
	// detectPool 专用于管理端「检测账号」，与额度同步/续期隔离，默认并发 32。
	detectPool             *batch.Pool
	credentialRefreshWake  chan struct{}
	autoCleanMu            sync.RWMutex
	autoClean              AutoCleanConfig
	autoCleanRevision      uint64
	autoCleanWake          chan struct{}
	excludeBuildBotFlagged bool
	buildBotFlagCache      *resultcache.Cache[string, []uint64]
	logger                 *slog.Logger
	now                    func() time.Time
}

func (s *Service) SetQuotaRecoveryQueue(queue repository.QuotaRecoveryQueue) {
	s.quotaQueue = queue
}

func (s *Service) SetQuotaRefreshCoordinator(value repository.QuotaRefreshCoordinator) {
	s.quotaRefreshState = value
}

func (s *Service) QuotaRefreshStats() QuotaRefreshStats {
	s.quotaRefreshMu.Lock()
	defer s.quotaRefreshMu.Unlock()
	result := QuotaRefreshStats{}
	for _, state := range s.quotaRefreshes {
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

func NewService(accounts repository.AccountRepository, audits repository.AuditRepository, deviceSessions repository.DeviceSessionRepository, sticky repository.StickySessionRepository, providers *provider.Registry, cipher security.Cryptor, refreshLock repository.DistributedLock) *Service {
	return &Service{
		accounts: accounts, audits: audits, deviceSessions: deviceSessions, sticky: sticky,
		providers: providers, cipher: cipher, refreshLock: refreshLock,
		quotaRefreshes:        make(map[string]*quotaRefreshState),
		quotaRefreshQueue:     make(chan quotaRefreshRequest, quotaRefreshQueueSize),
		quotaRefreshWake:      make(chan struct{}, 1),
		credentialRefreshWake: make(chan struct{}, 1),
		autoClean: AutoCleanConfig{
			Enabled: false, Interval: 10 * time.Minute, MinAge: time.Hour, IncludeDisabled: false,
		},
		autoCleanWake:     make(chan struct{}, 1),
		buildBotFlagCache: resultcache.New[string, []uint64](1, buildBotFlagCacheTTL),
		conversionPool:    batch.NewPool(25), syncPool: batch.NewPool(25), refreshPool: batch.NewPool(25), detectPool: batch.NewPool(32),
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

func (s *Service) List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]View, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	if (filter.Provider != "" && !accountdomain.Provider(filter.Provider).IsValid()) ||
		!oneOf(filter.QuotaType, "", "free", "paid", "unknown", "auto", "basic", "super", "heavy") ||
		!oneOf(filter.Status, "", "active", "disabled", "reauthRequired", "cooldown", "waitingReset", "probing", "risk") ||
		!oneOf(filter.Renewal, "", "refreshable", "unrefreshable") ||
		!oneOf(filter.Risk, "", "flagged", "normal") ||
		!oneOf(filter.Agreement, "", "nsfwEnabled", "nsfwDisabled", "termsAccepted", "termsNotAccepted", "allAccepted", "allNotAccepted") ||
		(filter.Agreement != "" && filter.Provider != string(accountdomain.ProviderWeb)) ||
		!validAssociationFilter(filter.Provider, filter.Association) ||
		!repository.IsValidSort(filter.Sort, "name", "type", "status", "createdAt") {
		return nil, 0, ErrInvalidFilter
	}
	var refreshable *bool
	if filter.Renewal != "" {
		value := filter.Renewal == "refreshable"
		refreshable = &value
	}
	repositoryFilter := repository.AccountListFilter{
		Provider: filter.Provider, QuotaType: filter.QuotaType, Status: filter.Status,
		Refreshable: refreshable, Agreement: filter.Agreement, Association: filter.Association, Now: s.now(),
	}
	if filter.Risk != "" {
		if _, ok := s.accounts.(buildBotFlagIndexRepository); ok {
			repositoryFilter.Risk = filter.Risk
		} else {
			flaggedIDs, err := s.buildBotFlaggedAccountIDs(ctx)
			if err != nil {
				return nil, 0, err
			}
			if filter.Risk == "flagged" {
				repositoryFilter.AccountIDs = flaggedIDs
				repositoryFilter.RestrictIDs = true
			} else {
				repositoryFilter.ExcludeIDs = flaggedIDs
			}
		}
	}
	values, total, err := s.accounts.List(ctx, repository.AccountListQuery{
		Page:   repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: search, Sort: filter.Sort},
		Filter: repositoryFilter,
	})
	if err != nil {
		return nil, 0, err
	}
	accountIDs := make([]uint64, 0, len(values))
	for _, value := range values {
		accountIDs = append(accountIDs, value.ID)
	}
	observedTokens, err := s.audits.SumTokensByAccountsSince(ctx, accountIDs, time.Now().UTC().Add(-freeUsageWindow))
	if err != nil {
		return nil, 0, err
	}
	billings, err := s.accounts.GetBillings(ctx, accountIDs)
	if err != nil {
		return nil, 0, err
	}
	recoveries, err := s.accounts.GetQuotaRecoveries(ctx, accountIDs)
	if err != nil {
		return nil, 0, err
	}
	quotaWindows, err := s.accounts.GetQuotaWindows(ctx, accountIDs)
	if err != nil {
		return nil, 0, err
	}
	views := make([]View, 0, len(values))
	for _, value := range values {
		metadata := s.buildBotFlagMetadata(value)
		view := View{Credential: value, BuildBotFlagged: metadata.BuildBotFlagged, BuildBotFlagSource: metadata.BuildBotFlagSource}
		if billing, ok := billings[value.ID]; ok {
			view.Billing = &billing
		}
		var recovery *accountdomain.QuotaRecovery
		if recoveryValue, ok := recoveries[value.ID]; ok {
			recovery = &recoveryValue
		}
		view.Quota = newQuotaView(view.Billing, observedTokens[value.ID], recovery, value.ObservedModel, value.BuildSuperEntitled && value.Provider == accountdomain.ProviderBuild)
		view.QuotaWindows = quotaWindows[value.ID]
		views = append(views, view)
	}
	return views, total, nil
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
	s.autoCleanMu.Lock()
	s.excludeBuildBotFlagged = value
	s.autoCleanMu.Unlock()
}

func (s *Service) excludeBuildBotFlaggedFromSchedulingEnabled() bool {
	s.autoCleanMu.RLock()
	defer s.autoCleanMu.RUnlock()
	return s.excludeBuildBotFlagged
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

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// validAssociationFilter validates association filters against the selected provider.
// Web keeps its six Build/Console/combined values; Build and Console filter only by Web links.
func validAssociationFilter(providerValue, association string) bool {
	if association == "" {
		return true
	}
	switch providerValue {
	case string(accountdomain.ProviderWeb):
		return oneOf(association, "buildLinked", "buildUnlinked", "consoleLinked", "consoleUnlinked", "allLinked", "allUnlinked")
	case string(accountdomain.ProviderBuild), string(accountdomain.ProviderConsole):
		return oneOf(association, "webLinked", "webUnlinked")
	default:
		return false
	}
}

// BatchUpdate 对同一号池的一组账号应用相同路由参数。
func (s *Service) BatchUpdate(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, input UpdateInput) (int64, error) {
	ids, err := normalizeIDs(ids, maxBatchUpdateAccounts)
	if err != nil {
		return 0, err
	}
	if !providerValue.IsValid() {
		return 0, invalidInput("账号来源无效")
	}
	slices.Sort(ids)
	if input.MaxConcurrent != nil && (*input.MaxConcurrent < 1 || *input.MaxConcurrent > accountdomain.MaxConcurrent) {
		return 0, invalidInput("maxConcurrent 必须在 1 到 256 之间")
	}
	if input.MinimumRemaining != nil && *input.MinimumRemaining < 0 {
		return 0, invalidInput("minimumRemaining 不能小于零")
	}
	if input.Name != nil {
		return 0, invalidInput("批量更新不支持修改账号名称")
	}
	updated, err := s.accounts.UpdateMany(ctx, providerValue, ids, repository.AccountUpdates{Enabled: input.Enabled, Priority: input.Priority, MaxConcurrent: input.MaxConcurrent, MinimumRemaining: input.MinimumRemaining})
	if err != nil {
		return 0, mapRepositoryError(err)
	}
	if input.Enabled != nil && !*input.Enabled && s.sticky != nil {
		if batchDeleter, ok := s.sticky.(repository.StickySessionBatchDeleter); ok {
			_ = batchDeleter.DeleteByAccounts(ctx, ids)
		} else {
			for _, id := range ids {
				_ = s.sticky.DeleteByAccount(ctx, id)
			}
		}
	}
	return updated, nil
}

// AccountDeleteResult summarizes a single/batch delete with optional linked peers.
type AccountDeleteResult struct {
	Deleted           int64
	RootsDeleted      int64
	LinkedDeleted     int64
	Skipped           int64
	DeletedByProvider map[accountdomain.Provider]int64
}

// accountDeleteResultFromOutcome converts repository results using rows actually deleted.
func accountDeleteResultFromOutcome(providerValue accountdomain.Provider, outcome repository.LinkedDeleteOutcome) AccountDeleteResult {
	out := AccountDeleteResult{
		Deleted:           outcome.Deleted,
		RootsDeleted:      outcome.RootsDeleted,
		LinkedDeleted:     outcome.Deleted - outcome.RootsDeleted,
		Skipped:           int64(len(outcome.SkippedRoots)),
		DeletedByProvider: map[accountdomain.Provider]int64{},
	}
	if providerValue.IsValid() && outcome.RootsDeleted > 0 {
		out.DeletedByProvider[providerValue] = outcome.RootsDeleted
	}
	for provider, count := range outcome.LinkedDeletedByProvider {
		out.DeletedByProvider[provider] += count
	}
	return out
}

// deleteStickyAccounts uses the optional batch capability and falls back for custom stores.
func (s *Service) deleteStickyAccounts(ctx context.Context, accountIDs []uint64) (int, error) {
	if s.sticky == nil || len(accountIDs) == 0 {
		return 0, nil
	}
	if batchDeleter, ok := s.sticky.(repository.StickySessionBatchDeleter); ok {
		if err := batchDeleter.DeleteByAccounts(ctx, accountIDs); err != nil {
			return len(accountIDs), err
		}
		return 0, nil
	}
	failures := 0
	var firstErr error
	for _, id := range accountIDs {
		if err := s.sticky.DeleteByAccount(ctx, id); err != nil {
			failures++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return failures, firstErr
}

// finishLinkedDelete clears runtime state after the database transaction commits.
func (s *Service) finishLinkedDelete(ctx context.Context, deletedIDs []uint64) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), linkedDeleteRuntimeCleanupLimit)
	defer cancel()
	if failures, err := s.deleteStickyAccounts(cleanupCtx, deletedIDs); err != nil && s.logger != nil {
		s.logger.Warn("linked_account_runtime_cleanup_failed", "accounts", len(deletedIDs), "failures", failures, "error", err)
	}
}

// BatchDelete atomically removes roots and quota state without expanding linked accounts.
func (s *Service) BatchDelete(ctx context.Context, ids []uint64) (int64, error) {
	result, err := s.batchDeleteWithLinkedMode(ctx, accountdomain.Provider(""), ids, nil, true)
	return result.Deleted, err
}

// BatchDeleteWithLinked deletes root accounts and optional linked peers resolved from binding tables.
// Roots with active video jobs are skipped together with their linked group; other groups are deleted.
func (s *Service) BatchDeleteWithLinked(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, targets []accountdomain.Provider) (AccountDeleteResult, error) {
	return s.batchDeleteWithLinkedMode(ctx, providerValue, ids, targets, true)
}

// batchDeleteWithLinkedMode is the shared atomic path; skipMedia selects reject-all or skip-group behavior.
func (s *Service) batchDeleteWithLinkedMode(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, targets []accountdomain.Provider, skipMedia bool) (AccountDeleteResult, error) {
	var out AccountDeleteResult
	ids, err := normalizeBatchIDs(ids)
	if err != nil {
		return out, err
	}
	if len(ids) == 0 {
		return out, nil
	}
	if len(targets) > 0 && !providerValue.IsValid() {
		return out, invalidInput("账号来源无效")
	}
	// Atomic path: lock roots → expand links → lock final → media handling → delete.
	outcome, err := s.accounts.DeleteManyWithLinked(ctx, providerValue, ids, targets, skipMedia)
	if err != nil {
		return out, mapLinkedDeleteError(err)
	}
	s.finishLinkedDelete(ctx, outcome.DeletedIDs)
	if outcome.Deleted > 0 {
		s.invalidateBuildBotFlagCache()
	}
	return accountDeleteResultFromOutcome(providerValue, outcome), nil
}

// AccountsBelongToProvider 校验批量账号是否全部属于指定号池。
// 该校验只读取账号主表，避免详情页的额度、审计或关联查询影响批量操作。
func (s *Service) AccountsBelongToProvider(ctx context.Context, ids []uint64, providerValue accountdomain.Provider) (bool, error) {
	if !providerValue.IsValid() {
		return false, invalidInput("账号来源无效")
	}
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return false, err
	}
	count, err := s.accounts.CountProviderAccountsByIDs(ctx, providerValue, values)
	if err != nil {
		return false, err
	}
	return count == int64(len(values)), nil
}

// CleanupResult summarizes rows deleted and root groups skipped by one cleanup operation.
type CleanupResult struct {
	Deleted           int64
	RootsDeleted      int64
	LinkedDeleted     int64
	Skipped           int64
	DeletedByProvider map[accountdomain.Provider]int64
}

// validateCleanupSelection validates cleanup states and linked target providers.
func validateCleanupSelection(providerValue accountdomain.Provider, statuses []CleanupStatus, targets []accountdomain.Provider) (map[CleanupStatus]struct{}, error) {
	if !providerValue.IsValid() {
		return nil, invalidInput("账号来源无效")
	}
	selected := make(map[CleanupStatus]struct{}, len(statuses))
	for _, status := range statuses {
		switch status {
		case CleanupStatusCooldown, CleanupStatusDisabled, CleanupStatusReauthRequired:
			selected[status] = struct{}{}
		default:
			return nil, invalidInput("账号清理状态无效")
		}
	}
	if len(selected) == 0 {
		return nil, invalidInput("至少选择一种账号状态")
	}
	for _, target := range targets {
		if !target.IsValid() {
			return nil, invalidInput("关联删除目标无效")
		}
		if target == providerValue {
			return nil, invalidInput("关联删除目标不能包含当前号池")
		}
	}
	return selected, nil
}

// CleanupAccounts deletes accounts in selected admin states; healthy, waiting-reset, and probing accounts are excluded.
// Linked targets are resolved from binding tables regardless of peer state, and active-media groups are skipped whole.
// The ID cursor always advances, so skipped groups cannot stall a cleanup batch.
func (s *Service) CleanupAccounts(ctx context.Context, providerValue accountdomain.Provider, statuses []CleanupStatus, targets []accountdomain.Provider) (CleanupResult, error) {
	out := CleanupResult{DeletedByProvider: map[accountdomain.Provider]int64{}}
	selected, err := validateCleanupSelection(providerValue, statuses, targets)
	if err != nil {
		return out, err
	}

	const cleanupBatchSize = 500
	now := s.now()
	for _, status := range []CleanupStatus{CleanupStatusDisabled, CleanupStatusReauthRequired, CleanupStatusCooldown} {
		if _, ok := selected[status]; !ok {
			continue
		}
		var afterID uint64
		for {
			outcome, candidates, maxID, err := s.accounts.DeleteAccountStatusBatchWithLinked(ctx, providerValue, string(status), now, afterID, cleanupBatchSize, targets)
			if err != nil {
				return out, mapLinkedDeleteError(err)
			}
			s.finishLinkedDelete(ctx, outcome.DeletedIDs)
			out.Deleted += outcome.Deleted
			out.RootsDeleted += outcome.RootsDeleted
			out.LinkedDeleted += outcome.Deleted - outcome.RootsDeleted
			out.Skipped += int64(len(outcome.SkippedRoots))
			if outcome.RootsDeleted > 0 {
				out.DeletedByProvider[providerValue] += outcome.RootsDeleted
			}
			for provider, count := range outcome.LinkedDeletedByProvider {
				out.DeletedByProvider[provider] += count
			}
			if candidates < cleanupBatchSize {
				break
			}
			afterID = maxID
		}
	}
	if out.Deleted > 0 {
		s.invalidateBuildBotFlagCache()
	}
	return out, nil
}

// PreviewCleanup returns root and linked-peer counts for the cleanup confirmation dialog.
// The preview is informational; deletion revalidates state inside each transaction.
func (s *Service) PreviewCleanup(ctx context.Context, providerValue accountdomain.Provider, statuses []CleanupStatus, targets []accountdomain.Provider) (repository.CleanupPreview, error) {
	selected, err := validateCleanupSelection(providerValue, statuses, targets)
	if err != nil {
		return repository.CleanupPreview{}, err
	}
	raw := make([]string, 0, len(selected))
	for _, status := range []CleanupStatus{CleanupStatusDisabled, CleanupStatusReauthRequired, CleanupStatusCooldown} {
		if _, ok := selected[status]; ok {
			raw = append(raw, string(status))
		}
	}
	preview, err := s.accounts.CountCleanupWithLinked(ctx, providerValue, raw, s.now(), targets)
	if err != nil {
		return repository.CleanupPreview{}, mapLinkedDeleteError(err)
	}
	return preview, nil
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
	observedTokens, err := s.audits.SumTokensByAccountsSince(ctx, []uint64{id}, time.Now().UTC().Add(-freeUsageWindow))
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

// ImportCredentials 导入用户上传的 OAuth 账号凭据。
func (s *Service) ImportCredentials(ctx context.Context, data []byte) (ImportResult, error) {
	return s.ImportCredentialsWithObserver(ctx, data, nil)
}

func (s *Service) ImportCredentialsWithObserver(ctx context.Context, data []byte, observer ImportedAccountObserver) (ImportResult, error) {
	return s.ImportCredentialsWithProgress(ctx, data, observer, nil)
}

// ImportCredentialsWithProgress 导入 Build 凭据并报告已写入流水线的账号数。
func (s *Service) ImportCredentialsWithProgress(ctx context.Context, data []byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.ImportCredentialDocumentsWithProgress(ctx, [][]byte{data}, observer, progress)
}

// ImportCredentialDocumentsWithProgress 合并解析多个 Build 凭据文件，并作为一个批次写入和同步。
func (s *Service) ImportCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(accountdomain.ProviderBuild)
	if !ok {
		return ImportResult{}, fmt.Errorf("CLI Provider 未注册")
	}
	return s.importCredentialDocumentsWithProgress(ctx, adapter, documents, observer, progress)
}

// ImportWebCredentials 导入版本化或旧号池格式的 Grok Web SSO 凭据。
func (s *Service) ImportWebCredentials(ctx context.Context, data []byte) (ImportResult, error) {
	return s.ImportWebCredentialsWithObserver(ctx, data, nil)
}

func (s *Service) ImportWebCredentialsWithObserver(ctx context.Context, data []byte, observer ImportedAccountObserver) (ImportResult, error) {
	return s.ImportWebCredentialsWithProgress(ctx, data, observer, nil)
}

// ImportWebCredentialsWithProgress 导入 Web 凭据并报告已写入流水线的账号数。
func (s *Service) ImportWebCredentialsWithProgress(ctx context.Context, data []byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.ImportWebCredentialDocumentsWithProgress(ctx, [][]byte{data}, observer, progress)
}

// ImportWebCredentialDocumentsWithProgress 合并解析多个 Web JSON 或 SSO 文本文件，并作为一个批次写入和同步。
func (s *Service) ImportWebCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(accountdomain.ProviderWeb)
	if !ok {
		return ImportResult{}, fmt.Errorf("Grok Web Provider 未注册")
	}
	return s.importCredentialDocumentsWithProgress(ctx, adapter, documents, observer, progress)
}

func (s *Service) ImportConsoleCredentials(ctx context.Context, data []byte) (ImportResult, error) {
	return s.ImportConsoleCredentialsWithObserver(ctx, data, nil)
}

func (s *Service) ImportConsoleCredentialsWithObserver(ctx context.Context, data []byte, observer ImportedAccountObserver) (ImportResult, error) {
	return s.ImportConsoleCredentialsWithProgress(ctx, data, observer, nil)
}

func (s *Service) ImportConsoleCredentialsWithProgress(ctx context.Context, data []byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.ImportConsoleCredentialDocumentsWithProgress(ctx, [][]byte{data}, observer, progress)
}

func (s *Service) ImportConsoleCredentialDocumentsWithProgress(ctx context.Context, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(accountdomain.ProviderConsole)
	if !ok {
		return ImportResult{}, fmt.Errorf("Grok Console Provider 未注册")
	}
	return s.importCredentialDocumentsWithProgress(ctx, adapter, documents, observer, progress)
}

func (s *Service) importCredentialDocumentsWithProgress(ctx context.Context, adapter provider.CredentialCodecAdapter, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	if len(documents) == 0 {
		return ImportResult{}, fmt.Errorf("%w: 没有可导入的账号文件", ErrInvalidImport)
	}
	seeds := make([]provider.CredentialSeed, 0)
	seen := make(map[string]struct{})
	parsedAccounts := 0
	skipped := 0
	for index, document := range documents {
		values, err := adapter.ParseImportedCredentials(document)
		if err != nil {
			if errors.Is(err, provider.ErrCredentialLimit) {
				return ImportResult{}, fmt.Errorf("%w: 单次最多导入 %d 个账号", ErrImportLimit, maxCredentialImportAccounts)
			}
			return ImportResult{}, fmt.Errorf("%w: 第 %d 个文件: %v", ErrInvalidImport, index+1, err)
		}
		parsedAccounts += len(values)
		if parsedAccounts > maxCredentialImportAccounts {
			return ImportResult{}, fmt.Errorf("%w: 单次最多导入 %d 个账号", ErrImportLimit, maxCredentialImportAccounts)
		}
		for _, value := range values {
			if value.SourceKey != "" {
				key := string(value.Provider) + "\x00" + value.SourceKey
				if _, exists := seen[key]; exists {
					skipped++
					continue
				}
				seen[key] = struct{}{}
			}
			seeds = append(seeds, value)
		}
	}
	// Preflight avoids unnecessary OAuth preparation for known deletions.
	// ImportAccounts rechecks current intent inside the actual write transaction.
	tombstoned, err := s.accounts.TombstonedEmails(ctx, seedEmails(seeds))
	if err != nil {
		return ImportResult{}, fmt.Errorf("读取账号墓碑失败: %w", err)
	}
	if len(tombstoned) > 0 {
		kept := seeds[:0]
		for _, seed := range seeds {
			if _, hit := tombstoned[accountdomain.ImportEmail(seed.Email)]; hit {
				skipped++
				if s.logger != nil {
					s.logger.Info("account_import_tombstoned_skipped", "email_hash", security.HashToken(seed.Email), "name", seed.Name)
				}
				continue
			}
			kept = append(kept, seed)
		}
		seeds = kept
	}
	var result ImportResult
	if preparer, ok := adapter.(provider.CredentialImportPreparer); ok && hasRefreshTokenOnlySeed(seeds) {
		result, err = s.persistPreparedImportedSeeds(ctx, seeds, preparer, observer, progress)
	} else {
		result, err = s.persistImportedSeeds(ctx, seeds, observer, progress)
	}
	result.Skipped += skipped
	return result, err
}

// seedEmails 提取导入种子的邮箱集合(去空)。
func seedEmails(seeds []provider.CredentialSeed) []string {
	out := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		if email := strings.TrimSpace(seed.Email); email != "" {
			out = append(out, email)
		}
	}
	return out
}

func hasRefreshTokenOnlySeed(seeds []provider.CredentialSeed) bool {
	for _, seed := range seeds {
		if strings.TrimSpace(seed.AccessToken) == "" && strings.TrimSpace(seed.RefreshToken) != "" {
			return true
		}
	}
	return false
}

func (s *Service) persistPreparedImportedSeeds(ctx context.Context, seeds []provider.CredentialSeed, preparer provider.CredentialImportPreparer, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	result := ImportResult{AccountIDs: make([]uint64, 0, len(seeds))}
	if progress != nil {
		if err := progress(0, len(seeds)); err != nil {
			return result, err
		}
	}
	prepareCtx, cancelPrepare := context.WithCancel(ctx)
	defer cancelPrepare()
	var (
		mu        sync.Mutex
		firstErr  error
		completed int
		persisted bool
		seen      = make(map[string]struct{}, len(seeds))
	)
	_, batchErr := batch.ForEachObserved(prepareCtx, seeds, batch.Options{Workers: credentialImportPrepareWorkers}, func(itemCtx context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
		if strings.TrimSpace(seed.AccessToken) == "" && strings.TrimSpace(seed.RefreshToken) != "" {
			return preparer.PrepareImportedCredential(itemCtx, seed)
		}
		return seed, nil
	}, func(index int, item batch.Result[provider.CredentialSeed]) {
		mu.Lock()
		defer mu.Unlock()
		completed++
		if !item.Completed || item.Err != nil {
			result.Failed++
			if item.Err != nil {
				s.logger.Warn("account_rt_import_failed", "index", index+1, "error", item.Err)
			}
			reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
			return
		}
		seed := item.Value
		if seed.SourceKey != "" {
			key := string(seed.Provider) + "\x00" + seed.SourceKey
			if _, exists := seen[key]; exists {
				result.Skipped++
				reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
				return
			}
			seen[key] = struct{}{}
		}

		// OAuth providers may invalidate the submitted refresh token as soon as
		// they return its replacement. Persist that replacement before any
		// request-scoped observer or progress callback can abort the import.
		persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
		stored, err := s.persistImportedSeed(persistCtx, seed)
		cancelPersist()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			cancelPrepare()
			return
		}
		if stored.Skipped != "" {
			result.Skipped++
			reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
			return
		}
		persisted = true
		result.AccountIDs = append(result.AccountIDs, stored.ID)
		if stored.Created {
			result.Created++
		} else {
			result.Updated++
		}
		if firstErr == nil && observer != nil {
			if err := observer(stored.ID); err != nil {
				firstErr = err
				cancelPrepare()
			}
		}
		reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
	})
	if persisted {
		s.invalidateBuildBotFlagCache()
		s.WakeCredentialRefresh()
	}
	return result, errors.Join(firstErr, batchErr)
}

func reportCredentialImportProgress(progress BatchProgressObserver, completed, total int, firstErr *error, cancel context.CancelFunc) {
	if progress == nil || *firstErr != nil {
		return
	}
	if err := progress(completed, total); err != nil {
		*firstErr = err
		cancel()
	}
}

func (s *Service) persistImportedSeed(ctx context.Context, seed provider.CredentialSeed) (repository.AccountUpsertResult, error) {
	value, err := s.credentialFromSeed(seed)
	if err != nil {
		return repository.AccountUpsertResult{}, err
	}
	stored, err := s.accounts.ImportAccounts(ctx, []repository.AccountImport{{Credential: value}})
	if err != nil {
		return repository.AccountUpsertResult{}, err
	}
	if len(stored) != 1 {
		return repository.AccountUpsertResult{}, fmt.Errorf("导入账号持久化结果数量无效: %d", len(stored))
	}
	if stored[0].Skipped == "" {
		s.reconcileProviderLinksBestEffort(ctx, stored[0].ID)
	}
	return stored[0], nil
}

func (s *Service) persistImportedSeeds(ctx context.Context, seeds []provider.CredentialSeed, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.persistImportedSeedsWithSources(ctx, seeds, nil, observer, progress)
}

func (s *Service) persistImportedSeedsWithSources(ctx context.Context, seeds []provider.CredentialSeed, sources []accountdomain.CredentialRef, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	if sources != nil && len(sources) != len(seeds) {
		return ImportResult{}, fmt.Errorf("导入账号与来源数量不一致")
	}
	completed, total := 0, len(seeds)
	result := ImportResult{AccountIDs: make([]uint64, 0, len(seeds))}
	defer func() {
		if result.Created+result.Updated > 0 {
			s.invalidateBuildBotFlagCache()
			s.WakeCredentialRefresh()
		}
	}()
	if progress != nil {
		if err := progress(completed, total); err != nil {
			return result, err
		}
	}
	for start := 0; start < len(seeds); start += credentialImportChunkSize {
		end := min(start+credentialImportChunkSize, len(seeds))
		values := make([]repository.AccountImport, 0, end-start)
		for index, seed := range seeds[start:end] {
			value, err := s.credentialFromSeed(seed)
			if err != nil {
				return result, err
			}
			input := repository.AccountImport{Credential: value}
			if sources != nil {
				ref := sources[start+index]
				input.Source = &ref
			}
			values = append(values, input)
		}
		stored, err := s.accounts.ImportAccounts(ctx, values)
		if err != nil {
			return result, err
		}
		if len(stored) != len(values) {
			return result, fmt.Errorf("导入账号持久化结果数量无效: %d", len(stored))
		}
		// The whole chunk committed before callbacks. Preserve all committed
		// counts even if an observer/progress callback cancels delivery.
		for _, value := range stored {
			if value.Skipped != "" {
				result.Skipped++
				continue
			}
			result.AccountIDs = append(result.AccountIDs, value.ID)
			if value.Created {
				result.Created++
			} else {
				result.Updated++
			}
		}
		for _, value := range stored {
			if value.Skipped == "" {
				s.reconcileProviderLinksBestEffort(ctx, value.ID)
				if observer != nil {
					if err := observer(value.ID); err != nil {
						return result, err
					}
				}
			}
			completed++
			if progress != nil {
				if err := progress(completed, total); err != nil {
					return result, err
				}
			}
		}
	}
	return result, nil
}

// SyncWebAccountsToConsoleWithProgress 使用 Web 账号的同一份 SSO 创建或更新 Console 账号。
func (s *Service) SyncWebAccountsToConsoleWithProgress(ctx context.Context, ids []uint64, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.SyncWebAccountsToConsoleWithStrategy(ctx, ids, WebConsoleSyncAll, observer, progress)
}

func (s *Service) SyncWebAccountsToConsoleWithStrategy(ctx context.Context, ids []uint64, strategy WebConsoleSyncStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	if strategy != WebConsoleSyncAll && strategy != WebConsoleSyncMissing {
		return ImportResult{}, invalidInput("Grok Web 到 Console 同步策略无效")
	}
	ids, err := normalizeIDs(ids, maxWebConsoleSyncAccounts)
	if err != nil {
		return ImportResult{}, err
	}
	if strategy == WebConsoleSyncMissing {
		values, err := s.accounts.ListMissingConsoleSyncAccounts(ctx, ids)
		if err != nil {
			return ImportResult{}, mapRepositoryError(err)
		}
		result, err := s.syncWebCredentialsToConsole(ctx, values, observer, progress)
		result.Skipped += len(ids) - len(values)
		return result, err
	}
	values := make([]accountdomain.Credential, 0, len(ids))
	for _, id := range ids {
		value, getErr := s.accounts.Get(ctx, id)
		if getErr != nil {
			return ImportResult{}, mapRepositoryError(getErr)
		}
		values = append(values, value)
	}
	return s.syncWebCredentialsToConsole(ctx, values, observer, progress)
}

// SyncAllWebAccountsToConsoleWithProgress 同步完整 Web 号池，避免前端分页遗漏账号。
func (s *Service) SyncAllWebAccountsToConsoleWithProgress(ctx context.Context, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.SyncAllWebAccountsToConsoleWithStrategy(ctx, WebConsoleSyncAll, observer, progress)
}

func (s *Service) SyncAllWebAccountsToConsoleWithStrategy(ctx context.Context, strategy WebConsoleSyncStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	if strategy != WebConsoleSyncAll && strategy != WebConsoleSyncMissing {
		return ImportResult{}, invalidInput("Grok Web 到 Console 同步策略无效")
	}
	batchSize := accountTaskBatchSize
	result := ImportResult{AccountIDs: make([]uint64, 0)}
	var afterID uint64
	completed := 0
	total := 0
	initialized := false
	for {
		var (
			values  []accountdomain.Credential
			count   int64
			skipped int64
			err     error
		)
		if strategy == WebConsoleSyncMissing {
			values, count, skipped, err = s.accounts.ListMissingConsoleSyncBatch(ctx, afterID, batchSize)
		} else {
			values, count, err = s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderWeb, afterID, batchSize)
		}
		if err != nil {
			return result, err
		}
		if !initialized {
			total = int(count)
			result.Skipped = int(skipped)
			initialized = true
			if progress != nil {
				if err := progress(0, total); err != nil {
					return result, err
				}
			}
		}
		if len(values) == 0 {
			return result, nil
		}
		current, err := s.syncWebCredentialsToConsole(ctx, values, observer, offsetBatchProgress(progress, completed, total))
		result.Created += current.Created
		result.Updated += current.Updated
		result.Skipped += current.Skipped
		result.Failed += current.Failed
		result.AccountIDs = append(result.AccountIDs, current.AccountIDs...)
		if err != nil {
			return result, err
		}
		completed += len(values)
		afterID = values[len(values)-1].ID
		if len(values) < batchSize {
			return result, nil
		}
	}
}

func (s *Service) syncWebCredentialsToConsole(ctx context.Context, values []accountdomain.Credential, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(accountdomain.ProviderConsole)
	if !ok {
		return ImportResult{}, fmt.Errorf("Grok Console Provider 未注册")
	}
	seeds := make([]provider.CredentialSeed, 0, len(values))
	sources := make([]accountdomain.CredentialRef, 0, len(values))
	for _, value := range values {
		if value.Provider != accountdomain.ProviderWeb || value.AuthType != accountdomain.AuthTypeSSO {
			return ImportResult{}, fmt.Errorf("%w: 仅 Grok Web SSO 账号支持同步到 Console", ErrUnsupported)
		}
		token, err := s.cipher.Decrypt(value.EncryptedAccessToken)
		if err != nil {
			return ImportResult{}, fmt.Errorf("解密 Grok Web SSO: %w", err)
		}
		// 非法 UTF-8 会被 json.Marshal 静默改写为 U+FFFD，显式拒绝优于静默改动（不应回显 token 内容）。
		if !utf8.ValidString(token) {
			return ImportResult{}, fmt.Errorf("解密 Grok Web SSO: 凭据不是合法 UTF-8")
		}
		// 内部调用固定走 JSON 对象路径，避免 plain token 被格式嗅探（如「[」JSON 保留前缀）误判。
		payload, err := json.Marshal(map[string]string{"sso_token": token})
		if err != nil {
			return ImportResult{}, fmt.Errorf("生成 Grok Console SSO 凭据: %w", err)
		}
		parsed, err := adapter.ParseImportedCredentials(payload)
		if err != nil {
			return ImportResult{}, fmt.Errorf("生成 Grok Console SSO 凭据: %w", err)
		}
		if len(parsed) != 1 {
			return ImportResult{}, fmt.Errorf("生成 Grok Console SSO 凭据: 预期 1 个账号，实际 %d 个", len(parsed))
		}
		seed := parsed[0]
		seed.Provider = accountdomain.ProviderConsole
		seed.AuthType = accountdomain.AuthTypeSSO
		seed.Name = webConsoleAccountName(value.Name, seed.Name)
		if strings.TrimSpace(value.EncryptedCloudflareCookie) != "" {
			cookies, decryptErr := s.cipher.Decrypt(value.EncryptedCloudflareCookie)
			if decryptErr != nil {
				return ImportResult{}, fmt.Errorf("解密 Grok Web Cloudflare Cookie: %w", decryptErr)
			}
			seed.CloudflareCookies = cookies
		}
		seeds = append(seeds, seed)
		sources = append(sources, value.CredentialRef())
	}
	return s.persistImportedSeedsWithSources(ctx, seeds, sources, observer, progress)
}

func webConsoleAccountName(webName, fallback string) string {
	name := strings.TrimSpace(webName)
	if name == "" {
		return fallback
	}
	if suffix, ok := strings.CutPrefix(name, "Grok Web "); ok {
		return "Grok Console " + suffix
	}
	return name
}

// ConvertWebAccountsToBuild 使用 Web SSO 自动完成 xAI Device Flow，并建立唯一的 Web/Build 账号关联。
func (s *Service) ConvertWebAccountsToBuild(ctx context.Context, ids []uint64) (BuildConversionResult, error) {
	return s.ConvertWebAccountsToBuildWithStrategy(ctx, ids, BuildConversionMissing, nil, nil)
}

func (s *Service) ConvertWebAccountsToBuildWithObserver(ctx context.Context, ids []uint64, observer ImportedAccountObserver) (BuildConversionResult, error) {
	return s.ConvertWebAccountsToBuildWithStrategy(ctx, ids, BuildConversionMissing, observer, nil)
}

// ConvertWebAccountsToBuildWithProgress 转换指定账号，并向调用方报告真实完成数。
func (s *Service) ConvertWebAccountsToBuildWithProgress(ctx context.Context, ids []uint64, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	return s.ConvertWebAccountsToBuildWithStrategy(ctx, ids, BuildConversionMissing, observer, progress)
}

func (s *Service) ConvertWebAccountsToBuildWithStrategy(ctx context.Context, ids []uint64, strategy BuildConversionStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	if strategy != BuildConversionAll && strategy != BuildConversionMissing {
		return BuildConversionResult{}, invalidInput("Grok Web 到 Build 转换策略无效")
	}
	ids, err := normalizeIDs(ids, maxBuildConversionAccounts)
	if err != nil {
		return BuildConversionResult{}, err
	}
	prefilteredSkipped := 0
	if strategy == BuildConversionMissing {
		candidates, err := s.accounts.FilterMissingBuildConversionIDs(ctx, ids)
		if err != nil {
			return BuildConversionResult{}, mapRepositoryError(err)
		}
		prefilteredSkipped = len(ids) - len(candidates)
		ids = candidates
	}
	result, err := s.convertWebAccountsToBuild(ctx, ids, strategy, observer, progress)
	result.Skipped += prefilteredSkipped
	return result, err
}

// ConvertAllWebAccountsToBuild 转换全部尚未建立 Build 关联的 Grok Web 账号。
func (s *Service) ConvertAllWebAccountsToBuild(ctx context.Context) (BuildConversionResult, error) {
	return s.ConvertAllWebAccountsToBuildWithStrategy(ctx, BuildConversionMissing, nil, nil)
}

func (s *Service) ConvertAllWebAccountsToBuildWithObserver(ctx context.Context, observer ImportedAccountObserver) (BuildConversionResult, error) {
	return s.ConvertAllWebAccountsToBuildWithStrategy(ctx, BuildConversionMissing, observer, nil)
}

// ConvertAllWebAccountsToBuildWithProgress 转换完整未关联号池，并向调用方报告真实完成数。
func (s *Service) ConvertAllWebAccountsToBuildWithProgress(ctx context.Context, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	return s.ConvertAllWebAccountsToBuildWithStrategy(ctx, BuildConversionMissing, observer, progress)
}

func (s *Service) ConvertAllWebAccountsToBuildWithStrategy(ctx context.Context, strategy BuildConversionStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	if strategy != BuildConversionAll && strategy != BuildConversionMissing {
		return BuildConversionResult{}, invalidInput("Grok Web 到 Build 转换策略无效")
	}
	batchSize := accountTaskBatchSize
	result := BuildConversionResult{BuildAccountIDs: make([]uint64, 0)}
	seenBuildIDs := make(map[uint64]struct{})
	var observed sync.Map
	batchObserver := observer
	if observer != nil {
		batchObserver = func(accountID uint64) error {
			if _, loaded := observed.LoadOrStore(accountID, struct{}{}); loaded {
				return nil
			}
			return observer(accountID)
		}
	}
	var afterID uint64
	completed := 0
	total := 0
	initialized := false
	for {
		var (
			ids   []uint64
			count int64
			err   error
		)
		if strategy == BuildConversionMissing {
			ids, count, err = s.accounts.ListUnlinkedWebAccountIDs(ctx, afterID, batchSize)
		} else {
			var values []accountdomain.Credential
			values, count, err = s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderWeb, afterID, batchSize)
			ids = make([]uint64, 0, len(values))
			for _, value := range values {
				ids = append(ids, value.ID)
			}
		}
		if err != nil {
			return result, err
		}
		if !initialized {
			total = int(count)
			initialized = true
			if progress != nil {
				if err := progress(0, total); err != nil {
					return result, err
				}
			}
		}
		if len(ids) == 0 {
			return result, nil
		}
		current, err := s.convertWebAccountsToBuild(ctx, ids, strategy, batchObserver, offsetBatchProgress(progress, completed, total))
		result.Created += current.Created
		result.Linked += current.Linked
		result.Skipped += current.Skipped
		result.Failed += current.Failed
		for _, buildID := range current.BuildAccountIDs {
			if _, exists := seenBuildIDs[buildID]; exists {
				continue
			}
			seenBuildIDs[buildID] = struct{}{}
			result.BuildAccountIDs = append(result.BuildAccountIDs, buildID)
		}
		if err != nil {
			return result, err
		}
		completed += len(ids)
		afterID = ids[len(ids)-1]
		if len(ids) < batchSize {
			return result, nil
		}
	}
}

func offsetBatchProgress(progress BatchProgressObserver, offset, total int) BatchProgressObserver {
	if progress == nil {
		return nil
	}
	return func(completed, _ int) error {
		if completed == 0 {
			return nil
		}
		return progress(offset+completed, total)
	}
}

func (s *Service) convertWebAccountsToBuild(ctx context.Context, ids []uint64, strategy BuildConversionStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	if progress != nil {
		if err := progress(0, len(ids)); err != nil {
			return BuildConversionResult{}, err
		}
	}
	type outcome struct {
		accountID uint64
		buildID   uint64
		created   bool
		skipped   bool
		err       error
	}
	var observed sync.Map
	var observerMu sync.Mutex
	var observerErr error
	completed := 0
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results, summary, runErr := batch.MapObserved(runCtx, ids, batch.Options{Workers: s.conversionPool.Limit(), Pool: s.conversionPool}, func(workCtx context.Context, id uint64) (outcome, error) {
		buildID, created, skipped, convertErr := s.convertWebAccountToBuild(workCtx, id, strategy)
		return outcome{accountID: id, buildID: buildID, created: created, skipped: skipped, err: convertErr}, nil
	}, func(_ int, execution batch.Result[outcome]) {
		observerMu.Lock()
		defer observerMu.Unlock()
		defer func() {
			completed++
			if progress != nil {
				if err := progress(completed, len(ids)); err != nil && observerErr == nil {
					observerErr = err
					cancel()
				}
			}
		}()
		item := execution.Value
		if execution.Err != nil || item.err != nil || item.skipped || observer == nil {
			return
		}
		if _, loaded := observed.LoadOrStore(item.buildID, struct{}{}); loaded {
			return
		}
		if err := observer(item.buildID); err != nil {
			if observerErr == nil {
				observerErr = err
				cancel()
			}
		}
	})
	s.logBatchSummary("web_to_build", s.conversionPool, summary, runErr)
	result := BuildConversionResult{BuildAccountIDs: make([]uint64, 0, len(ids))}
	seen := make(map[uint64]struct{}, len(ids))
	for index, execution := range results {
		item := execution.Value
		if execution.Err != nil {
			item.accountID = ids[index]
			item.err = execution.Err
		}
		if item.err != nil {
			result.Failed++
			s.logger.Warn("web_account_build_conversion_failed", "account_id", item.accountID, "error", item.err)
			continue
		}
		if item.skipped {
			result.Skipped++
			continue
		}
		if item.created {
			result.Created++
		} else {
			result.Linked++
		}
		if _, ok := seen[item.buildID]; !ok {
			seen[item.buildID] = struct{}{}
			result.BuildAccountIDs = append(result.BuildAccountIDs, item.buildID)
		}
	}
	if runErr != nil {
		return result, runErr
	}
	if observerErr != nil {
		return result, observerErr
	}
	return result, nil
}

func (s *Service) convertWebAccountToBuild(ctx context.Context, id uint64, strategy BuildConversionStrategy) (uint64, bool, bool, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return 0, false, false, mapRepositoryError(err)
	}
	if value.Provider != accountdomain.ProviderWeb || value.AuthType != accountdomain.AuthTypeSSO {
		return 0, false, false, ErrUnsupported
	}
	if value.LinkedAccountID != 0 && strategy == BuildConversionMissing {
		return value.LinkedAccountID, false, true, nil
	}
	release, acquired, err := s.refreshLock.Acquire(ctx, "web-build-conversion:"+strconv.FormatUint(id, 10), 2*time.Minute)
	if err != nil {
		return 0, false, false, err
	}
	if !acquired {
		return 0, false, false, ErrConversionBusy
	}
	defer release()
	value, err = s.accounts.Get(ctx, id)
	if err != nil {
		return 0, false, false, mapRepositoryError(err)
	}
	if value.LinkedAccountID != 0 && strategy == BuildConversionMissing {
		return value.LinkedAccountID, false, true, nil
	}
	linkedBuildSourceKey := ""
	var target *accountdomain.CredentialRef
	if value.LinkedAccountID != 0 {
		linkedBuild, getErr := s.accounts.Get(ctx, value.LinkedAccountID)
		if getErr != nil {
			return 0, false, false, mapRepositoryError(getErr)
		}
		if linkedBuild.Provider != accountdomain.ProviderBuild || strings.TrimSpace(linkedBuild.SourceKey) == "" {
			return 0, false, false, fmt.Errorf("已关联 Grok Build 账号身份无效")
		}
		linkedBuildSourceKey = linkedBuild.SourceKey
		reference := linkedBuild.CredentialRef()
		target = &reference
	}
	converter, ok := s.providers.BuildConverter(accountdomain.ProviderWeb)
	if !ok {
		return 0, false, false, fmt.Errorf("Grok Web SSO 转换能力未注册")
	}
	seed, err := converter.ConvertToBuild(ctx, value)
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			err = errors.Join(err, s.markSSOCredentialRejected(ctx, value, "Grok Web SSO credential rejected"))
		}
		return 0, false, false, err
	}
	seed.Provider = accountdomain.ProviderBuild
	seed.AuthType = accountdomain.AuthTypeOAuth
	if linkedBuildSourceKey != "" {
		seed.SourceKey = linkedBuildSourceKey
	}
	source := value.CredentialRef()
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	installed, err := s.persistSeed(persistCtx, seed, &source, target)
	if err != nil {
		return 0, false, false, err
	}
	if installed.Skipped == accountdomain.ImportTargetChanged {
		return 0, false, false, fmt.Errorf("%w: 已关联账号材料已变化，请重新转换", ErrConflict)
	}
	if installed.Skipped != "" {
		return 0, false, true, nil
	}
	if err := s.accounts.LinkWebToBuild(persistCtx, source, installed.Material); err != nil {
		return installed.ID, installed.Created, false, fmt.Errorf("凭据已保存，但账号关联未完成: %w", mapRepositoryError(err))
	}
	return installed.ID, installed.Created, false, nil
}

// ExportCredentials 保留 Grok Build 默认导出语义，供旧调用方兼容。
func (s *Service) ExportCredentials(ctx context.Context) (ExportResult, error) {
	return s.ExportProviderCredentials(ctx, accountdomain.ProviderBuild)
}

// ExportProviderCredentials 导出可由对应 Provider 导入接口重新读取的凭据文档。
func (s *Service) ExportProviderCredentials(ctx context.Context, providerValue accountdomain.Provider) (ExportResult, error) {
	return s.exportProviderCredentials(ctx, providerValue, repository.AccountListQuery{
		Page:   repository.PageQuery{Limit: maxCredentialExportAccounts + 1},
		Filter: repository.AccountListFilter{Provider: string(providerValue), Now: s.now()},
	}, true, 0)
}

// ExportProviderCredentialsCursor exports a stable provider batch bounded by
// the maximum account ID captured by the first request.
func (s *Service) ExportProviderCredentialsCursor(ctx context.Context, providerValue accountdomain.Provider, afterID, snapshotMaxID uint64, limit int) (ExportPageResult, error) {
	if limit < 1 || limit > maxCredentialExportAccounts {
		return ExportPageResult{}, invalidInput("单批导出数量必须在 1 到 10000 之间")
	}
	if afterID > 0 && snapshotMaxID == 0 {
		return ExportPageResult{}, invalidInput("继续导出时必须提供快照上界")
	}
	if snapshotMaxID > 0 && afterID > snapshotMaxID {
		return ExportPageResult{}, invalidInput("导出游标不能超过快照上界")
	}
	if !providerValue.IsValid() {
		return ExportPageResult{}, invalidInput("账号来源无效")
	}
	if snapshotMaxID == 0 {
		values, _, err := s.accounts.List(ctx, repository.AccountListQuery{
			Page:   repository.PageQuery{Limit: 1, Sort: repository.SortQuery{Field: "id", Direction: repository.SortDescending}},
			Filter: repository.AccountListFilter{Provider: string(providerValue), Now: s.now()},
		})
		if err != nil {
			return ExportPageResult{}, err
		}
		if len(values) == 0 {
			result, exportErr := s.marshalProviderCredentials(providerValue, nil)
			return ExportPageResult{ExportResult: result}, exportErr
		}
		snapshotMaxID = values[0].ID
	}
	values, total, err := s.accounts.List(ctx, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: limit, Sort: repository.SortQuery{Field: "id", Direction: repository.SortAscending}},
		Filter: repository.AccountListFilter{
			Provider: string(providerValue), AfterID: afterID, ThroughID: snapshotMaxID, Now: s.now(),
		},
	})
	if err != nil {
		return ExportPageResult{}, err
	}
	result, err := s.marshalProviderCredentials(providerValue, values)
	if err != nil {
		return ExportPageResult{}, err
	}
	nextID := afterID
	if len(values) > 0 {
		nextID = values[len(values)-1].ID
	}
	return ExportPageResult{
		ExportResult: result, NextID: nextID, SnapshotMaxID: snapshotMaxID, HasMore: total > int64(len(values)),
	}, nil
}

// ExportProviderCredentialsByIDs 只导出管理端明确选择且属于指定 Provider 的账号。
func (s *Service) ExportProviderCredentialsByIDs(ctx context.Context, providerValue accountdomain.Provider, ids []uint64) (ExportResult, error) {
	values, err := normalizeIDs(ids, maxCredentialExportAccounts)
	if err != nil {
		return ExportResult{}, err
	}
	return s.exportProviderCredentials(ctx, providerValue, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: len(values)},
		Filter: repository.AccountListFilter{
			Provider: string(providerValue), AccountIDs: values, RestrictIDs: true, Now: s.now(),
		},
	}, false, len(values))
}

func (s *Service) exportProviderCredentials(ctx context.Context, providerValue accountdomain.Provider, query repository.AccountListQuery, enforceTotalLimit bool, expectedCount int) (ExportResult, error) {
	if !providerValue.IsValid() {
		return ExportResult{}, invalidInput("账号来源无效")
	}
	values, total, err := s.accounts.List(ctx, query)
	if err != nil {
		return ExportResult{}, err
	}
	if enforceTotalLimit && total > maxCredentialExportAccounts {
		return ExportResult{}, fmt.Errorf("%w: 单次最多导出 10000 个账号", ErrExportLimit)
	}
	if err := validateCredentialExportCount(expectedCount, total, len(values)); err != nil {
		return ExportResult{}, err
	}
	return s.marshalProviderCredentials(providerValue, values)
}

func validateCredentialExportCount(expected int, total int64, actual int) error {
	if expected > 0 && (total != int64(expected) || actual != expected) {
		return invalidInput("所选账号包含不存在或不属于当前号池的账号")
	}
	return nil
}

func (s *Service) marshalProviderCredentials(providerValue accountdomain.Provider, values []accountdomain.Credential) (ExportResult, error) {
	if !providerValue.IsValid() {
		return ExportResult{}, invalidInput("账号来源无效")
	}
	if s.providers == nil {
		return ExportResult{}, fmt.Errorf("Provider 注册表未初始化")
	}
	adapter, ok := s.providers.CredentialCodec(providerValue)
	if !ok {
		return ExportResult{}, fmt.Errorf("Provider %s 不支持凭据导出", providerValue)
	}
	var err error
	seeds := make([]provider.CredentialSeed, 0, len(values))
	for _, value := range values {
		if value.Provider != providerValue {
			continue
		}
		accessToken := ""
		if value.EncryptedAccessToken != "" {
			accessToken, err = s.cipher.Decrypt(value.EncryptedAccessToken)
			if err != nil {
				return ExportResult{}, fmt.Errorf("解密账号 %d access token: %w", value.ID, err)
			}
		}
		refreshToken := ""
		if value.EncryptedRefreshToken != "" {
			refreshToken, err = s.cipher.Decrypt(value.EncryptedRefreshToken)
			if err != nil {
				return ExportResult{}, fmt.Errorf("解密账号 %d refresh token: %w", value.ID, err)
			}
		}
		cloudflareCookies := ""
		if value.EncryptedCloudflareCookie != "" {
			cloudflareCookies, err = s.cipher.Decrypt(value.EncryptedCloudflareCookie)
			if err != nil {
				return ExportResult{}, fmt.Errorf("解密账号 %d Cloudflare Cookie: %w", value.ID, err)
			}
		}
		if accessToken == "" && refreshToken == "" {
			return ExportResult{}, fmt.Errorf("账号 %d 没有可导出的凭据", value.ID)
		}
		seeds = append(seeds, provider.CredentialSeed{
			Provider: value.Provider, AuthType: value.AuthType, WebTier: value.WebTier,
			Name: value.Name, Email: value.Email, UserID: value.UserID, TeamID: value.TeamID,
			OIDCClientID: value.OIDCClientID, AccessToken: accessToken, RefreshToken: refreshToken,
			CloudflareCookies: cloudflareCookies, ExpiresAt: value.ExpiresAt,
			WebNSFWEnabledAt: value.WebNSFWEnabledAt, WebTermsAcceptedAt: value.WebTermsAcceptedAt,
			WebTermsAcceptedVersion: value.WebTermsAcceptedVersion, WebBirthDateSetAt: value.WebBirthDateSetAt,
		})
	}
	data, err := adapter.MarshalCredentials(seeds)
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Data: data, Count: len(seeds)}, nil
}

func (s *Service) Update(ctx context.Context, id uint64, input UpdateInput) (View, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	patch := repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{
		Enabled: input.Enabled, Priority: input.Priority, MaxConcurrent: input.MaxConcurrent, MinimumRemaining: input.MinimumRemaining,
	}, BuildSuperEntitled: input.BuildSuperEntitled, BuildRouteMode: input.BuildRouteMode}
	if input.Name != nil {
		value.Name = strings.TrimSpace(*input.Name)
		if value.Name == "" {
			return View{}, invalidInput("账号名称不能为空")
		}
		patch.Name = &value.Name
	}
	if input.MaxConcurrent != nil {
		if *input.MaxConcurrent < 1 || *input.MaxConcurrent > accountdomain.MaxConcurrent {
			return View{}, invalidInput("maxConcurrent 必须在 1 到 256 之间")
		}
	}
	if input.MinimumRemaining != nil {
		if *input.MinimumRemaining < 0 {
			return View{}, invalidInput("minimumRemaining 不能小于零")
		}
	}
	if input.RiskStatus != nil {
		status := strings.TrimSpace(*input.RiskStatus)
		if status != "" && status != accountdomain.RiskStatusRSCDenied {
			return View{}, invalidInput("riskStatus 仅支持空值或 rsc_denied")
		}
		patch.Risk = &repository.RiskAttribution{Status: status}
		if status != "" {
			patch.Risk.Trigger = accountdomain.RiskTriggerManual
		}
	}
	if input.ClearCloudflareCookies {
		value.EncryptedCloudflareCookie = ""
		patch.EncryptedCloudflareCookie = &value.EncryptedCloudflareCookie
	} else if input.CloudflareCookies != nil {
		if value.Provider == accountdomain.ProviderBuild {
			return View{}, invalidInput("Grok Build 账号不使用 Cloudflare Cookie")
		}
		if len(*input.CloudflareCookies) > 16<<10 {
			return View{}, invalidInput("Cloudflare Cookie 不能超过 16 KiB")
		}
		if strings.TrimSpace(*input.CloudflareCookies) != "" {
			cookies := cfcookies.Sanitize(*input.CloudflareCookies)
			if cookies == "" {
				return View{}, invalidInput("Cloudflare Cookie 中没有有效字段")
			}
			encrypted, encryptErr := s.cipher.Encrypt(cookies)
			if encryptErr != nil {
				return View{}, encryptErr
			}
			value.EncryptedCloudflareCookie = encrypted
			patch.EncryptedCloudflareCookie = &value.EncryptedCloudflareCookie
		}
	}
	if input.BuildSuperEntitled != nil {
		if value.Provider != accountdomain.ProviderBuild {
			return View{}, invalidInput("仅 Grok Build 账号支持设置 Build Super entitlement")
		}
	}
	if input.BuildRouteMode != nil {
		if value.Provider != accountdomain.ProviderBuild {
			return View{}, invalidInput("仅 Grok Build 账号支持设置上游地址")
		}
		if !input.BuildRouteMode.IsValid() {
			return View{}, invalidInput("Build 上游地址必须是 auto、build 或 xai")
		}
	}
	result, err := s.accounts.UpdateAdministration(ctx, id, patch)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	updated := result.Credential
	if !updated.Enabled && s.sticky != nil {
		_ = s.sticky.DeleteByAccount(ctx, updated.ID)
	} else if updated.Enabled && s.providers != nil && s.providers.SupportsCredentialRefresh(updated.Provider) {
		s.WakeCredentialRefresh()
	}
	view, err := s.Get(ctx, updated.ID)
	if err != nil {
		return View{}, err
	}
	view.EnabledChanged = result.EnabledChanged
	return view, nil
}

// ClearCooldown applies an explicit administrator command against current health.
func (s *Service) ClearCooldown(ctx context.Context, id uint64) (View, error) {
	if _, err := s.accounts.ApplyHealth(ctx, id, "", accountdomain.HealthEvent{Kind: accountdomain.HealthClearCooldown}); err != nil {
		return View{}, mapRepositoryError(err)
	}
	return s.Get(ctx, id)
}

// MarkBuildAPIFallback 幂等写入 Build 账号 XAI 推理回退标记；失败不吞掉，调用方可重试。
func (s *Service) MarkBuildAPIFallback(ctx context.Context, id uint64, enabled bool) error {
	return mapRepositoryError(s.accounts.MarkBuildAPIFallback(ctx, id, enabled))
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	// Single-account delete must preserve ErrNotFound when the root row is gone
	// (BatchDeleteWithLinked/DeleteMany return deleted=0, nil for missing IDs).
	result, err := s.DeleteWithLinked(ctx, accountdomain.Provider(""), id, nil)
	if err != nil {
		return err
	}
	if result.Deleted == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWithLinked deletes one account and optional linked peers.
// A single delete is rejected if any account in the final group has an active video job.
func (s *Service) DeleteWithLinked(ctx context.Context, providerValue accountdomain.Provider, id uint64, targets []accountdomain.Provider) (AccountDeleteResult, error) {
	if id == 0 {
		return AccountDeleteResult{}, invalidInput("账号 ID 无效")
	}
	result, err := s.batchDeleteWithLinkedMode(ctx, providerValue, []uint64{id}, targets, false)
	if err != nil {
		return result, err
	}
	// Fail closed for the single-root API: missing root must not report success.
	if result.Deleted == 0 {
		return result, ErrNotFound
	}
	return result, nil
}

// PreviewLinkedDelete returns root/linked counts for the delete confirmation UI.
func (s *Service) PreviewLinkedDelete(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, targets []accountdomain.Provider) (repository.LinkedDeleteResolution, error) {
	ids, err := normalizeBatchIDs(ids)
	if err != nil {
		return repository.LinkedDeleteResolution{}, err
	}
	if !providerValue.IsValid() {
		return repository.LinkedDeleteResolution{}, invalidInput("账号来源无效")
	}
	resolution, err := s.accounts.ResolveLinkedDeleteIDs(ctx, providerValue, ids, targets)
	if err != nil {
		return repository.LinkedDeleteResolution{}, mapLinkedDeleteError(err)
	}
	return resolution, nil
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
	result, err := s.refreshes.Do(ctx, refreshKey, func() (any, error) {
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
	value = strings.Map(func(char rune) rune {
		switch char {
		case '\r', '\n', '\t':
			return ' '
		}
		if char < 0x20 || char == 0x7f {
			return -1
		}
		return char
	}, strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 512 {
		value = string(runes[:511]) + "…"
	}
	return value
}

func normalizeCredentialRefreshErrorResponse(value string) string {
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, strings.TrimSpace(value))
	runes := []rune(value)
	if len(runes) > 4096 {
		value = string(runes[:4095]) + "…"
	}
	return value
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
	return !accountdomain.IsPermanentCredentialRefreshErrorCode(code)
}

func (s *Service) RefreshBilling(ctx context.Context, id uint64) (accountdomain.Billing, error) {
	result, err := s.billingSyncs.Do(ctx, strconv.FormatUint(id, 10), func() (any, error) {
		return s.refreshBilling(ctx, id)
	})
	if err != nil {
		return accountdomain.Billing{}, err
	}
	billing, ok := result.(accountdomain.Billing)
	if !ok {
		return accountdomain.Billing{}, fmt.Errorf("额度同步返回类型无效")
	}
	return billing, nil
}

func (s *Service) refreshBilling(ctx context.Context, id uint64) (accountdomain.Billing, error) {
	value, billing, err := s.fetchBilling(ctx, id)
	if err != nil {
		return accountdomain.Billing{}, err
	}
	result, err := s.commitBilling(ctx, value.QuotaRecoveryRef(), billing, false)
	if err != nil {
		return accountdomain.Billing{}, err
	}
	if !result.Applied {
		return accountdomain.Billing{}, fmt.Errorf("%w: 额度状态已更新，请重新同步", ErrConflict)
	}
	return billing, nil
}

// fetchBilling captures the material and recovery revision before upstream IO.
func (s *Service) fetchBilling(ctx context.Context, id uint64) (accountdomain.Credential, accountdomain.Billing, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return value, accountdomain.Billing{}, mapRepositoryError(err)
	}
	refreshed, err := s.EnsureCredential(ctx, value, false)
	if err != nil {
		return value, accountdomain.Billing{}, err
	}
	value = refreshed
	adapter, ok := s.providers.Billing(value.Provider)
	if !ok {
		return value, accountdomain.Billing{}, fmt.Errorf("Provider %s 未注册", value.Provider)
	}
	billing, err := adapter.GetBilling(ctx, value)
	billing.AccountID = id
	return value, billing, err
}

func (s *Service) commitBilling(ctx context.Context, ref accountdomain.QuotaRecoveryRef, billing accountdomain.Billing, afterProbe bool) (accountdomain.RecoveryResult, error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	return s.accounts.ApplyQuotaRecovery(writeCtx, ref, accountdomain.RecoveryEvent{Kind: accountdomain.RecoveryBillingObserved, Billing: &billing, AfterProbe: afterProbe, OccurredAt: s.now()})
}

// ProbePaidQuota completes only its claimed revision, even if credential refresh
// loaded a newer account snapshot. Promotion returns the committed revision for
// the next model request; obsolete results never promote a lease.
func (s *Service) ProbePaidQuota(ctx context.Context, value accountdomain.Credential, ref accountdomain.QuotaRecoveryRef) (accountdomain.Credential, bool, error) {
	latest, billing, err := s.fetchBilling(ctx, value.ID)
	if latest.ID != 0 {
		ref.CredentialRef = latest.CredentialRef()
	}
	if err != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
		defer cancel()
		_, writeErr := s.accounts.ApplyQuotaRecovery(writeCtx, ref, accountdomain.RecoveryEvent{Kind: accountdomain.RecoveryPaidProbeFailed, OccurredAt: s.now()})
		return value, false, errors.Join(err, writeErr)
	}
	result, err := s.commitBilling(ctx, ref, billing, true)
	if err != nil {
		return value, false, err
	}
	if !result.Applied {
		return value, false, accountdomain.ErrQuotaRecoveryObservationStale
	}
	latest.QuotaRecoveryRevision, latest.QuotaRecoveryResetRevision = result.Ref.Revision, result.ResetRevision
	return latest, result.Recovered, nil
}

// HasBillingSnapshot 判断账号是否已经完成过一次额度同步，不触发任何上游请求。
func (s *Service) HasBillingSnapshot(ctx context.Context, id uint64) (bool, error) {
	_, err := s.accounts.GetBilling(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) HasQuotaWindows(ctx context.Context, id uint64) (bool, error) {
	return s.accounts.HasQuotaWindows(ctx, id)
}

func (s *Service) ExhaustQuota(ctx context.Context, id uint64, mode string, resetAt *time.Time) error {
	if resetAt == nil {
		windows, err := s.accounts.GetQuotaWindows(ctx, []uint64{id})
		if err == nil {
			for _, window := range windows[id] {
				if window.Mode != mode {
					continue
				}
				resetAt = quotaRecoveryDueAt(window, s.now(), true)
				break
			}
		}
	}
	if err := s.accounts.ExhaustQuotaWindow(ctx, id, mode, resetAt, s.now()); err != nil {
		return err
	}
	if resetAt != nil && s.quotaQueue != nil {
		return s.quotaQueue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: id, Mode: mode, DueAt: *resetAt})
	}
	return nil
}

func (s *Service) ExhaustWebQuota(ctx context.Context, id uint64, mode string, resetAt *time.Time) error {
	return s.ExhaustQuota(ctx, id, mode, resetAt)
}

func (s *Service) RefreshQuota(ctx context.Context, id uint64) ([]accountdomain.QuotaWindow, error) {
	result, err := s.quotaSyncs.Do(ctx, "all:"+strconv.FormatUint(id, 10), func() (any, error) {
		return s.refreshQuota(ctx, id)
	})
	if err != nil {
		return nil, err
	}
	refreshed, ok := result.(quotaRefreshResult)
	if !ok {
		return nil, fmt.Errorf("Provider 额度同步返回类型无效")
	}
	if err := s.reconcileQuotaRecoveryWindows(ctx, refreshed.Credential.Provider, id, refreshed.Windows); err != nil {
		return refreshed.Windows, err
	}
	// 身份补全是非关键操作：只在额度落库和恢复任务调度完成后执行，
	// 并沿用调用方取消语义，不能反向影响额度同步结果。
	value := refreshed.Credential
	if (value.Provider == accountdomain.ProviderWeb || value.Provider == accountdomain.ProviderConsole) && ctx.Err() == nil {
		// SyncAccountIdentity 会自行判断身份是否完整。Web 账号必须具备合法
		// Gateway UUID，不能因为旧记录里只有 email 就跳过迁移。
		if identityErr := s.syncAccountIdentityBestEffort(ctx, id); errors.Is(identityErr, provider.ErrUnauthorized) {
			return refreshed.Windows, identityErr
		}
	}
	return refreshed.Windows, nil
}

func (s *Service) RefreshWebQuota(ctx context.Context, id uint64) ([]accountdomain.QuotaWindow, error) {
	return s.RefreshQuota(ctx, id)
}

func (s *Service) refreshQuota(ctx context.Context, id uint64) (quotaRefreshResult, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, mapRepositoryError(err)
	}
	adapter, ok := s.providers.Quota(value.Provider)
	if !ok {
		return quotaRefreshResult{}, fmt.Errorf("%s Quota Provider 未注册", value.Provider)
	}
	revision, err := s.accounts.GetQuotaRevision(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, err
	}
	snapshot, err := adapter.SyncQuota(ctx, value)
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			err = errors.Join(err, s.markSSOCredentialRejected(ctx, value, fmt.Sprintf("%s SSO credential rejected", value.Provider)))
		}
		return quotaRefreshResult{}, err
	}
	quotaKind, _ := s.providers.QuotaKind(value.Provider)
	if quotaKind == provider.QuotaLocalWindow {
		existing, loadErr := s.accounts.GetQuotaWindows(ctx, []uint64{id})
		if loadErr != nil {
			return quotaRefreshResult{}, loadErr
		}
		snapshot.Windows = preserveActiveQuotaWindows(existing[id], snapshot.Windows, s.now())
	}
	if err := s.saveQuotaSnapshot(ctx, value.Provider, repository.QuotaSnapshotWrite{AccountID: id, Revision: revision, Tier: snapshot.Tier, SyncedAt: snapshot.SyncedAt, Windows: snapshot.Windows, ReplaceAll: true}); err != nil {
		return quotaRefreshResult{}, err
	}
	return quotaRefreshResult{Credential: value, Windows: snapshot.Windows}, nil
}

func preserveActiveQuotaWindows(existing, incoming []accountdomain.QuotaWindow, now time.Time) []accountdomain.QuotaWindow {
	byMode := make(map[string]accountdomain.QuotaWindow, len(existing))
	for _, window := range existing {
		byMode[window.Mode] = window
	}
	result := append([]accountdomain.QuotaWindow(nil), incoming...)
	for index, window := range result {
		current, ok := byMode[window.Mode]
		if !ok || current.ResetAt == nil || !current.ResetAt.After(now) {
			continue
		}
		result[index] = current
	}
	return result
}

// ReconcileRateLimit 根据额度模式核实 429。Web 周池和 Console
// 均以上游快照为准；Console 的 resource-exhausted 还可能表示瞬时
// RPS/RPM 限流，不能在未查询 /usage 时直接将账号冻结 24 小时。
func (s *Service) ReconcileRateLimit(ctx context.Context, id uint64, mode string, retryAfter time.Duration) (RateLimitReconcileState, error) {
	if mode == "weekly" || isConsoleUsageQuotaMode(mode) {
		var window accountdomain.QuotaWindow
		var err error
		if isConsoleUsageQuotaMode(mode) {
			window, err = s.refreshConsoleQuotaModeLocked(ctx, id, mode)
		} else {
			window, err = s.RefreshQuotaMode(ctx, id, mode)
		}
		if err != nil {
			if isConsoleUsageQuotaMode(mode) {
				if errors.Is(err, errQuotaRefreshBusy) {
					return RateLimitReconcileRefreshing, nil
				}
				// Keep the last known snapshot on an inconclusive probe and let the
				// bounded refresh worker retry. The gateway will still apply its normal
				// account cooldown and rotate this request to another account.
				s.QueueQuotaRefresh(id, mode)
				s.logger.Warn("console_rate_limit_quota_probe_failed", "account_id", id, "mode", mode, "error", err)
			}
			return RateLimitReconcileInconclusive, err
		}
		if window.Remaining == 0 || window.UsagePercent >= 100 {
			return RateLimitReconcileExhausted, nil
		}
		return RateLimitReconcileAvailable, nil
	}
	var resetAt *time.Time
	if retryAfter > 0 {
		value := s.now().Add(retryAfter)
		resetAt = &value
	}
	if err := s.ExhaustQuota(ctx, id, mode, resetAt); err != nil {
		return RateLimitReconcileInconclusive, err
	}
	return RateLimitReconcileExhausted, nil
}

func (s *Service) ReconcileWebRateLimit(ctx context.Context, id uint64, mode string, retryAfter time.Duration) (bool, error) {
	state, err := s.ReconcileRateLimit(ctx, id, mode, retryAfter)
	return state == RateLimitReconcileExhausted, err
}

func (s *Service) RefreshQuotaMode(ctx context.Context, id uint64, mode string) (accountdomain.QuotaWindow, error) {
	mode = strings.TrimSpace(mode)
	key := quotaSyncKey(id, mode)
	result, err := s.quotaSyncs.Do(ctx, key, func() (any, error) {
		if isWebImagineQuotaMode(mode) {
			return s.refreshQuotaGroup(ctx, id, accountdomain.QuotaGroupWebImagine)
		}
		return s.refreshQuotaMode(ctx, id, mode)
	})
	if err != nil {
		return accountdomain.QuotaWindow{}, err
	}
	refreshed, ok := result.(quotaRefreshResult)
	if !ok {
		return accountdomain.QuotaWindow{}, fmt.Errorf("Provider 模式额度同步返回类型无效")
	}
	if len(refreshed.Modes) > 0 {
		if err := s.reconcileQuotaGroupWindows(ctx, refreshed.Credential.Provider, id, refreshed.Modes, refreshed.Windows); err != nil {
			return accountdomain.QuotaWindow{}, err
		}
	}
	window, err := s.resolveRefreshedQuotaWindow(ctx, id, mode, refreshed)
	if err != nil {
		return accountdomain.QuotaWindow{}, err
	}
	if len(refreshed.Modes) == 0 && refreshed.Credential.Provider == accountdomain.ProviderConsole {
		// One Console request refreshes all three authoritative windows. Reconcile
		// every matching recovery event so externally consumed media quota cannot
		// remain unscheduled merely because a different kind triggered the refresh.
		if err := s.reconcileQuotaRecoveryWindows(ctx, refreshed.Credential.Provider, id, refreshed.Windows); err != nil {
			return window, err
		}
	} else if len(refreshed.Modes) == 0 {
		if err := s.reconcileQuotaRecoveryWindow(ctx, refreshed.Credential.Provider, id, window); err != nil {
			return window, err
		}
	} else if window.Mode == "weekly" {
		// The requested Imagine product was availability-only and resolved to
		// the paid shared pool. Reconcile the authoritative weekly window too;
		// the group reconciliation above only covers product-specific modes.
		if err := s.reconcileQuotaRecoveryWindow(ctx, refreshed.Credential.Provider, id, window); err != nil {
			return window, err
		}
	}
	return window, nil
}

// ProbeQuotaMode refreshes a claimed recovery event without scheduling a
// second event for the same account and mode. The recovery worker owns the
// current claim and is responsible for acknowledging or rescheduling it.
func (s *Service) ProbeQuotaMode(ctx context.Context, id uint64, mode string) (accountdomain.QuotaWindow, error) {
	mode = strings.TrimSpace(mode)
	key := quotaSyncKey(id, mode)
	result, err := s.quotaSyncs.Do(ctx, key, func() (any, error) {
		if isWebImagineQuotaMode(mode) {
			return s.refreshQuotaGroup(ctx, id, accountdomain.QuotaGroupWebImagine)
		}
		return s.refreshQuotaMode(ctx, id, mode)
	})
	if err != nil {
		return accountdomain.QuotaWindow{}, err
	}
	refreshed, ok := result.(quotaRefreshResult)
	if !ok {
		return accountdomain.QuotaWindow{}, fmt.Errorf("Provider 模式额度探测返回类型无效")
	}
	return s.resolveRefreshedQuotaWindow(ctx, id, mode, refreshed)
}

func (s *Service) resolveRefreshedQuotaWindow(ctx context.Context, id uint64, mode string, refreshed quotaRefreshResult) (accountdomain.QuotaWindow, error) {
	if window, ok := quotaWindowByMode(refreshed.Windows, mode); ok {
		return window, nil
	}
	credential := refreshed.Credential
	paidWebImagine := credential.Provider == accountdomain.ProviderWeb && isWebImagineQuotaMode(mode) &&
		(credential.WebTier == accountdomain.WebTierSuper || credential.WebTier == accountdomain.WebTierHeavy)
	if paidWebImagine {
		weekly, err := s.refreshQuotaMode(ctx, id, "weekly")
		if err != nil {
			return accountdomain.QuotaWindow{}, err
		}
		if window, ok := quotaWindowByMode(weekly.Windows, "weekly"); ok {
			return window, nil
		}
	}
	return accountdomain.QuotaWindow{}, fmt.Errorf("Provider usage 响应缺少 %s 额度", mode)
}

func (s *Service) refreshQuotaGroup(ctx context.Context, id uint64, group string) (quotaRefreshResult, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, mapRepositoryError(err)
	}
	adapter, ok := s.providers.QuotaGroup(value.Provider)
	if !ok {
		return quotaRefreshResult{}, fmt.Errorf("%s quota group Provider 未注册", value.Provider)
	}
	revision, err := s.accounts.GetQuotaRevision(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, err
	}
	snapshot, err := adapter.SyncQuotaGroup(ctx, value, group)
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			err = errors.Join(err, s.markSSOCredentialRejected(ctx, value, fmt.Sprintf("%s SSO credential rejected", value.Provider)))
		}
		return quotaRefreshResult{}, err
	}
	if snapshot.Group != group || len(snapshot.Modes) == 0 {
		return quotaRefreshResult{}, fmt.Errorf("Provider quota group %s 返回无效快照", group)
	}
	if snapshot.SyncedAt.IsZero() {
		snapshot.SyncedAt = s.now()
	}
	if err := s.saveQuotaSnapshot(ctx, value.Provider, repository.QuotaSnapshotWrite{AccountID: id, Revision: revision, SyncedAt: snapshot.SyncedAt, Windows: snapshot.Windows, ReplaceModes: snapshot.Modes}); err != nil {
		return quotaRefreshResult{}, err
	}
	return quotaRefreshResult{Credential: value, Windows: snapshot.Windows, Modes: snapshot.Modes}, nil
}

func (s *Service) refreshQuotaMode(ctx context.Context, id uint64, mode string) (quotaRefreshResult, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, mapRepositoryError(err)
	}
	// 停用/待重登账号不再发起上游探测(与 credential_scheduler 的纪律对齐):
	// 每次探测都消耗出口与 SSO 请求, 401 还会触发状态写入。
	if !value.Enabled || value.AuthStatus == accountdomain.AuthStatusReauthRequired {
		return quotaRefreshResult{Credential: value}, nil
	}
	adapter, ok := s.providers.Quota(value.Provider)
	if !ok {
		return quotaRefreshResult{}, fmt.Errorf("%s Quota Provider 未注册", value.Provider)
	}
	revision, err := s.accounts.GetQuotaRevision(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, err
	}
	var window accountdomain.QuotaWindow
	var windows []accountdomain.QuotaWindow
	var syncedAt time.Time
	var tier accountdomain.WebTier
	if value.Provider == accountdomain.ProviderConsole {
		// Console /usage always returns Chat, Image and Video together. Persist the
		// response as one authoritative snapshot so each media route observes the
		// same upstream usage generation.
		var snapshot provider.QuotaSnapshot
		snapshot, err = adapter.SyncQuota(ctx, value)
		if err == nil {
			windows = snapshot.Windows
			syncedAt = snapshot.SyncedAt
			for _, candidate := range windows {
				if candidate.Mode == mode {
					window = candidate
					break
				}
			}
			if window.Mode == "" {
				err = fmt.Errorf("Console usage 响应缺少 %s 额度", mode)
			}
		}
	} else {
		window, err = adapter.SyncQuotaMode(ctx, value, mode)
		windows = []accountdomain.QuotaWindow{window}
		syncedAt = s.now()
	}
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			err = errors.Join(err, s.markSSOCredentialRejected(ctx, value, fmt.Sprintf("%s SSO credential rejected", value.Provider)))
		}
		return quotaRefreshResult{}, err
	}
	quotaKind, _ := s.providers.QuotaKind(value.Provider)
	if quotaKind == provider.QuotaRemoteWindow {
		// Web reconciliation updates one mode; Console already supplied and
		// persisted its complete /usage snapshot above.
		tier = value.WebTier
	}
	if syncedAt.IsZero() {
		syncedAt = s.now()
	}
	if err := s.saveQuotaSnapshot(ctx, value.Provider, repository.QuotaSnapshotWrite{AccountID: id, Revision: revision, Tier: tier, SyncedAt: syncedAt, Windows: windows, ReplaceAll: value.Provider == accountdomain.ProviderConsole}); err != nil {
		return quotaRefreshResult{}, err
	}
	return quotaRefreshResult{Credential: value, Windows: windows}, nil
}

func quotaSyncKey(accountID uint64, mode string) string {
	mode = strings.TrimSpace(mode)
	if isConsoleUsageQuotaMode(mode) {
		return "all:" + strconv.FormatUint(accountID, 10)
	}
	if isWebImagineQuotaMode(mode) || mode == accountdomain.QuotaGroupWebImagine {
		return accountdomain.QuotaGroupWebImagine + ":" + strconv.FormatUint(accountID, 10)
	}
	return mode + ":" + strconv.FormatUint(accountID, 10)
}

func quotaWindowByMode(windows []accountdomain.QuotaWindow, mode string) (accountdomain.QuotaWindow, bool) {
	for _, window := range windows {
		if window.Mode == mode {
			return window, true
		}
	}
	return accountdomain.QuotaWindow{}, false
}

func (s *Service) reconcileQuotaRecoveryWindows(ctx context.Context, providerValue accountdomain.Provider, accountID uint64, windows []accountdomain.QuotaWindow) error {
	for _, window := range windows {
		if err := s.reconcileQuotaRecoveryWindow(ctx, providerValue, accountID, window); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcileQuotaGroupWindows(ctx context.Context, providerValue accountdomain.Provider, accountID uint64, modes []string, windows []accountdomain.QuotaWindow) error {
	byMode := make(map[string]accountdomain.QuotaWindow, len(windows))
	for _, window := range windows {
		byMode[window.Mode] = window
	}
	for _, mode := range modes {
		if window, ok := byMode[mode]; ok {
			if err := s.reconcileQuotaRecoveryWindow(ctx, providerValue, accountID, window); err != nil {
				return err
			}
			continue
		}
		if s.quotaQueue != nil {
			if err := s.quotaQueue.CancelQuotaRecovery(ctx, accountID, mode); err != nil {
				return fmt.Errorf("取消额度恢复事件: %w", err)
			}
		}
	}
	return nil
}

func (s *Service) reconcileQuotaRecoveryWindow(ctx context.Context, providerValue accountdomain.Provider, accountID uint64, window accountdomain.QuotaWindow) error {
	if s.quotaQueue == nil || !quotaWindowControlsRouting(providerValue, window.Mode) {
		return nil
	}
	if dueAt := quotaRecoveryDueAt(window, s.now(), window.Remaining == 0); dueAt != nil {
		if err := s.quotaQueue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: accountID, Mode: window.Mode, DueAt: *dueAt}); err != nil {
			return fmt.Errorf("安排额度恢复事件: %w", err)
		}
		return nil
	}
	if err := s.quotaQueue.CancelQuotaRecovery(ctx, accountID, window.Mode); err != nil {
		return fmt.Errorf("取消额度恢复事件: %w", err)
	}
	return nil
}

func (s *Service) ListDueWebQuotaWindows(ctx context.Context, now time.Time, limit int) ([]accountdomain.QuotaWindow, error) {
	windows, err := s.ListDueQuotaWindows(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	result := make([]accountdomain.QuotaWindow, 0, len(windows))
	for _, window := range windows {
		credential, getErr := s.accounts.Get(ctx, window.AccountID)
		if errors.Is(getErr, repository.ErrNotFound) {
			continue
		}
		if getErr != nil {
			return nil, getErr
		}
		if credential.Provider == accountdomain.ProviderWeb {
			result = append(result, window)
		}
	}
	return result, nil
}

func (s *Service) ListDueQuotaWindows(ctx context.Context, now time.Time, limit int) ([]accountdomain.QuotaWindow, error) {
	return s.accounts.ListDueQuotaWindows(ctx, now, limit)
}

func isWebChatQuotaMode(mode string) bool {
	switch mode {
	case "auto", "fast", "expert", "heavy":
		return true
	default:
		return false
	}
}

func isConsoleUsageQuotaMode(mode string) bool {
	switch mode {
	case "console", "console_image", "console_video":
		return true
	default:
		return false
	}
}

func isWebImagineQuotaMode(mode string) bool {
	return accountdomain.IsWebImagineQuotaMode(mode)
}

func quotaWindowControlsRouting(providerValue accountdomain.Provider, mode string) bool {
	return providerValue != accountdomain.ProviderConsole || isConsoleUsageQuotaMode(mode)
}

// SyncAllBilling 尽力刷新全部启用账号，单个账号失败不阻断其他账号。
func (s *Service) SyncAllBilling(ctx context.Context) (int, int, error) {
	return s.SyncAllBillingWithProgress(ctx, nil)
}

func (s *Service) SyncAllBillingWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, error) {
	if s.providers == nil {
		return 0, 0, fmt.Errorf("Provider 注册表未初始化")
	}
	ids := make([]uint64, 0)
	for _, providerValue := range s.providers.Providers() {
		quotaKind, ok := s.providers.QuotaKind(providerValue)
		if !ok || quotaKind != provider.QuotaBilling {
			continue
		}
		providerIDs, err := s.accounts.ListEnabledAccountIDs(ctx, providerValue, false)
		if err != nil {
			return 0, 0, err
		}
		ids = append(ids, providerIDs...)
	}
	return s.refreshBillings(ctx, ids, progress)
}

// SyncAllWebQuotas 尽力同步全部启用 Grok Web 账号的分模式额度。
func (s *Service) SyncAllWebQuotas(ctx context.Context) (int, int, error) {
	return s.SyncAllWebQuotasWithProgress(ctx, nil)
}

func (s *Service) SyncAllWebQuotasWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, error) {
	return s.syncAllQuotasWithProgress(ctx, accountdomain.ProviderWeb, "web_quota_sync", progress)
}

func (s *Service) SyncAllConsoleQuotas(ctx context.Context) (int, int, error) {
	return s.SyncAllConsoleQuotasWithProgress(ctx, nil)
}

func (s *Service) SyncAllConsoleQuotasWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, error) {
	return s.syncAllQuotasWithProgress(ctx, accountdomain.ProviderConsole, "console_quota_sync", progress)
}

// SyncIncompleteConsoleQuotas replaces pre-/usage synthetic windows and
// partial snapshots without refreshing accounts that already have all three
// authoritative Console quota kinds. It is safe to run periodically and uses
// the shared sync pool to preserve the deployment-wide upstream limit.
func (s *Service) SyncIncompleteConsoleQuotas(ctx context.Context) (int, int, error) {
	const batchSize = 1000
	var succeeded, failed int
	var afterID uint64
	for {
		values, _, err := s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderConsole, afterID, batchSize)
		if err != nil {
			return succeeded, failed, err
		}
		if len(values) == 0 {
			return succeeded, failed, nil
		}
		ids := make([]uint64, 0, len(values))
		for _, value := range values {
			if value.Enabled && value.AuthStatus == accountdomain.AuthStatusActive {
				ids = append(ids, value.ID)
			}
		}
		windows, err := s.accounts.GetQuotaWindows(ctx, ids)
		if err != nil {
			return succeeded, failed, err
		}
		pending := make([]uint64, 0, len(ids))
		for _, id := range ids {
			if !completeConsoleUsageSnapshot(windows[id]) {
				pending = append(pending, id)
			}
		}
		var batchSucceeded, batchFailed int
		if len(pending) > 0 {
			batchSucceeded, batchFailed, err = s.syncConsoleQuotaAccounts(ctx, "console_usage_migration", pending)
		}
		succeeded += batchSucceeded
		failed += batchFailed
		if err != nil {
			return succeeded, failed, err
		}
		afterID = values[len(values)-1].ID
		if len(values) < batchSize {
			return succeeded, failed, nil
		}
	}
}

// SyncStaleConsoleQuotas refreshes a bounded batch of complete but old /usage
// snapshots. Active accounts are already refreshed after successful requests;
// this catch-up covers idle pools without turning a large deployment into a
// periodic upstream request burst.
func (s *Service) SyncStaleConsoleQuotas(ctx context.Context, before time.Time, afterID uint64, limit int) (int, int, uint64, error) {
	if limit <= 0 || limit > accountTaskBatchSize {
		limit = 50
	}
	pending := make([]uint64, 0, limit)
	nextAfterID := afterID
	reachedEnd := false
	for len(pending) < limit {
		values, _, err := s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderConsole, nextAfterID, accountTaskBatchSize)
		if err != nil {
			return 0, 0, nextAfterID, err
		}
		if len(values) == 0 {
			reachedEnd = true
			break
		}
		ids := make([]uint64, 0, len(values))
		for _, value := range values {
			if value.Enabled && value.AuthStatus == accountdomain.AuthStatusActive {
				ids = append(ids, value.ID)
			}
		}
		windows, err := s.accounts.GetQuotaWindows(ctx, ids)
		if err != nil {
			return 0, 0, nextAfterID, err
		}
		for _, value := range values {
			nextAfterID = value.ID
			if value.Enabled && value.AuthStatus == accountdomain.AuthStatusActive && staleCompleteConsoleUsageSnapshot(windows[value.ID], before) {
				pending = append(pending, value.ID)
				if len(pending) == limit {
					break
				}
			}
		}
		if len(values) < accountTaskBatchSize {
			reachedEnd = len(pending) < limit
			break
		}
	}
	if reachedEnd {
		nextAfterID = 0
	}
	if len(pending) == 0 {
		return 0, 0, nextAfterID, nil
	}
	succeeded, failed, err := s.syncConsoleQuotaAccounts(ctx, "console_quota_stale_catchup", pending)
	return succeeded, failed, nextAfterID, err
}

func staleCompleteConsoleUsageSnapshot(windows []accountdomain.QuotaWindow, before time.Time) bool {
	if !completeConsoleUsageSnapshot(windows) {
		return false
	}
	for _, window := range windows {
		if !isConsoleUsageQuotaMode(window.Mode) {
			continue
		}
		if window.SyncedAt == nil || window.SyncedAt.Before(before) {
			return true
		}
	}
	return false
}

func (s *Service) syncConsoleQuotaAccounts(ctx context.Context, operation string, ids []uint64) (int, int, error) {
	return s.runAccountBatch(ctx, operation, ids, s.syncPool, nil, func(workCtx context.Context, id uint64) error {
		_, refreshErr := s.refreshConsoleQuotaModeLocked(workCtx, id, "console")
		if errors.Is(refreshErr, errQuotaRefreshBusy) {
			// Another replica already owns the refresh. Treat that as accepted work:
			// queuing the same account locally only creates a trailing duplicate probe.
			return nil
		}
		if refreshErr != nil && workCtx.Err() == nil {
			s.QueueQuotaRefresh(id, "console")
		}
		return refreshErr
	})
}

func (s *Service) refreshConsoleQuotaModeLocked(ctx context.Context, id uint64, mode string) (accountdomain.QuotaWindow, error) {
	var release func()
	if s.refreshLock != nil {
		acquiredRelease, acquired, err := s.refreshLock.Acquire(ctx, consoleQuotaRefreshLockKey(id), 2*quotaRefreshTimeout)
		if err != nil {
			return accountdomain.QuotaWindow{}, err
		}
		if !acquired {
			return accountdomain.QuotaWindow{}, errQuotaRefreshBusy
		}
		release = acquiredRelease
		defer release()
	}
	return s.RefreshQuotaMode(ctx, id, mode)
}

func consoleQuotaRefreshLockKey(id uint64) string {
	return "quota-refresh:console:" + strconv.FormatUint(id, 10)
}

func completeConsoleUsageSnapshot(windows []accountdomain.QuotaWindow) bool {
	var present uint8
	for _, window := range windows {
		if window.Source != accountdomain.QuotaSourceUpstream || window.SyncedAt == nil {
			continue
		}
		switch window.Mode {
		case "console":
			present |= 1
		case "console_image":
			present |= 2
		case "console_video":
			present |= 4
		}
	}
	return present == 7
}

func (s *Service) syncAllQuotasWithProgress(ctx context.Context, providerValue accountdomain.Provider, operation string, progress BatchProgressObserver) (int, int, error) {
	ids, err := s.accounts.ListEnabledAccountIDs(ctx, providerValue, false)
	if err != nil {
		return 0, 0, err
	}
	return s.runAccountBatch(ctx, operation, ids, s.syncPool, progress, func(workCtx context.Context, id uint64) error {
		_, err := s.RefreshQuota(workCtx, id)
		return err
	})
}

// SyncWebQuotaAccounts 同步指定 Web 账号集合，供启动追赶任务复用共享并发池。
func (s *Service) SyncWebQuotaAccounts(ctx context.Context, ids []uint64) (int, int, error) {
	return s.runAccountBatch(ctx, "web_quota_startup_catchup", ids, s.syncPool, nil, func(workCtx context.Context, id uint64) error {
		_, err := s.RefreshWebQuota(workCtx, id)
		return err
	})
}

// RefreshAllTokens 续期所有声明支持刷新的 Provider 凭据，不可续期账号会被跳过。
func (s *Service) RefreshAllTokens(ctx context.Context) (int, int, int, error) {
	return s.RefreshAllTokensWithProgress(ctx, nil)
}

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

// BatchRefreshBilling 使用有限并发刷新选中账号，避免大量账号同步时串行阻塞或无界创建 goroutine。
func (s *Service) BatchRefreshBilling(ctx context.Context, ids []uint64) (int, int, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, 0, err
	}
	return s.refreshBillings(ctx, values, nil)
}

// BatchResetQuotaState clears local Build quota recovery and exhausted-model
// blocks. Health, auth, risk, model access restrictions, billing and audit
// history have independent owners and remain in force.
func (s *Service) BatchResetQuotaState(ctx context.Context, ids []uint64) (int, error) {
	values, err := normalizeIDs(ids, maxQuotaResetAccounts)
	if err != nil {
		return 0, err
	}
	for start := 0; start < len(values); start += quotaResetChunkSize {
		end := min(start+quotaResetChunkSize, len(values))
		count, countErr := s.accounts.CountProviderAccountsByIDs(ctx, accountdomain.ProviderBuild, values[start:end])
		if countErr != nil {
			return 0, countErr
		}
		if count != int64(end-start) {
			return 0, invalidInput("仅 Grok Build 账号支持手动重置额度状态")
		}
	}
	reset := 0
	for start := 0; start < len(values); start += quotaResetChunkSize {
		if err := ctx.Err(); err != nil {
			return reset, err
		}
		end := min(start+quotaResetChunkSize, len(values))
		if err := s.accounts.ResetQuotaState(ctx, accountdomain.ProviderBuild, values[start:end]); err != nil {
			return reset, err
		}
		reset += end - start
	}
	return reset, nil
}

// ResetAllBuildQuotaState clears local quota state for every enabled Build
// account with active authentication, without materializing the complete ID set
// in memory. Other restrictions remain in force (see BatchResetQuotaState).
func (s *Service) ResetAllBuildQuotaState(ctx context.Context) (int64, error) {
	return s.accounts.ResetProviderQuotaState(ctx, accountdomain.ProviderBuild, true)
}

// BatchRefreshQuota 使用有限并发同步选中 Web 或 Console 账号的额度窗口。
func (s *Service) BatchRefreshQuota(ctx context.Context, ids []uint64) (int, int, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, 0, err
	}
	return s.runAccountBatch(ctx, "quota_sync", values, s.syncPool, nil, func(workCtx context.Context, id uint64) error {
		_, err := s.RefreshQuota(workCtx, id)
		return err
	})
}

func (s *Service) refreshBillings(ctx context.Context, ids []uint64, progress BatchProgressObserver) (int, int, error) {
	return s.runAccountBatch(ctx, "billing_sync", ids, s.syncPool, progress, func(workCtx context.Context, id uint64) error {
		_, err := s.RefreshBilling(workCtx, id)
		return err
	})
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
		sourceKey = "device:" + security.HashToken(seed.AccessToken)
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
		value.EgressIdentity = "sso_" + security.HashToken(seed.AccessToken)[:32]
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

// DecryptedAccessToken exposes one account's primary token for internal
// risk checks (RSC). Callers must never log or persist the returned value.
func (s *Service) DecryptedAccessToken(ctx context.Context, id uint64) (string, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return "", mapRepositoryError(err)
	}
	if value.EncryptedAccessToken == "" {
		return "", ErrInvalidInput
	}
	return s.cipher.Decrypt(value.EncryptedAccessToken)
}

// LinkedWebAccountID resolves the Web SSO identity behind a Build account.
func (s *Service) LinkedWebAccountID(ctx context.Context, buildAccountID uint64) (uint64, bool, error) {
	linked, ok := s.accounts.(interface {
		LinkedWebAccountID(context.Context, uint64) (uint64, bool, error)
	})
	if !ok {
		return 0, false, nil
	}
	return linked.LinkedWebAccountID(ctx, buildAccountID)
}

// LinkedBuildAccountIDs lists Build accounts sharing one Web identity.
func (s *Service) LinkedBuildAccountIDs(ctx context.Context, webAccountID uint64) ([]uint64, error) {
	linked, ok := s.accounts.(interface {
		LinkedBuildAccountIDs(context.Context, uint64) ([]uint64, error)
	})
	if !ok {
		return nil, nil
	}
	return linked.LinkedBuildAccountIDs(ctx, webAccountID)
}

// LinkedConsoleAccountIDs lists Console accounts sharing one Web identity.
// SSO patrol denials fan out to this group; request-path attribution does not.
func (s *Service) LinkedConsoleAccountIDs(ctx context.Context, webAccountID uint64) ([]uint64, error) {
	linked, ok := s.accounts.(interface {
		LinkedConsoleAccountIDs(context.Context, uint64) ([]uint64, error)
	})
	if !ok {
		return nil, nil
	}
	return linked.LinkedConsoleAccountIDs(ctx, webAccountID)
}

// SetAccountRiskAttribution writes the risk attribution columns for one
// account. 旧 risk 服务与仲裁院执行器已删除(切换手册第2步);risk_status
// 列的人工读写(PATCH /accounts/:id)照旧,自动归因写入仅剩本方法的
// 显式调用方。
func (s *Service) SetAccountRiskAttribution(ctx context.Context, id uint64, flagged bool, trigger string, originAccountID uint64, detail string, checkedAt time.Time) error {
	status := ""
	var at *time.Time
	if flagged {
		status = accountdomain.RiskStatusRSCDenied
		if trigger == "" {
			trigger = accountdomain.RiskTriggerDegrade
		}
		if !checkedAt.IsZero() {
			t := checkedAt.UTC()
			at = &t
		}
	} else {
		trigger = ""
		originAccountID = 0
		detail = ""
	}
	if len(detail) > 512 {
		detail = detail[:512]
	}
	if err := s.accounts.UpdateRiskAttribution(ctx, id, repository.RiskAttribution{
		Status: status, Trigger: trigger, OriginAccountID: originAccountID, CheckedAt: at, Detail: detail,
	}); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

// ClearMissingThinkingCooldown lifts missing-thinking and quality-idle
// cooldowns when a risk verdict proves the degrade was not account-scoped.
// Generic 5xx/429 penalties stay: a clean verdict does not prove the account
// is healthy.
func (s *Service) ClearMissingThinkingCooldown(ctx context.Context, id uint64) error {
	return s.ClearMissingThinkingCooldownAfterGrace(ctx, id, 0)
}

// The minimum hold and marker are evaluated atomically with the clear command.
func (s *Service) ClearMissingThinkingCooldownAfterGrace(ctx context.Context, id uint64, minHold time.Duration) error {
	_, err := s.accounts.ApplyHealth(ctx, id, "", accountdomain.HealthEvent{Kind: accountdomain.HealthClearQuality, MinHold: minHold})
	return mapRepositoryError(err)
}
