package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	"github.com/chenyme/grok2api/backend/internal/application/adminauth"
	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	invalidationapp "github.com/chenyme/grok2api/backend/internal/application/invalidation"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	quotarecoveryapp "github.com/chenyme/grok2api/backend/internal/application/quotarecovery"
	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	"github.com/chenyme/grok2api/backend/internal/buildinfo"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	inframedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/mediafetch"
	"github.com/chenyme/grok2api/backend/internal/infra/observability"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	updatecheckinfra "github.com/chenyme/grok2api/backend/internal/infra/updatecheck"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	qualitycourt "github.com/chenyme/grok2api/backend/internal/quality/court"
	qualityenforcement "github.com/chenyme/grok2api/backend/internal/quality/enforcement"
	"github.com/chenyme/grok2api/backend/internal/quality/events"
	qualityevidence "github.com/chenyme/grok2api/backend/internal/quality/evidence"
	qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"
	qualityinvestigator "github.com/chenyme/grok2api/backend/internal/quality/investigator"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	qualitymanagement "github.com/chenyme/grok2api/backend/internal/quality/management"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	qualityproxy "github.com/chenyme/grok2api/backend/internal/quality/proxy"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/repository"
	httpserver "github.com/chenyme/grok2api/backend/internal/transport/http"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	httpmiddleware "github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	qualityhttp "github.com/chenyme/grok2api/backend/internal/transport/http/quality"
)

// Application 管理后端进程生命周期和本地后台任务。
type Application struct {
	lifecycleMu        sync.Mutex
	closeMu            sync.Mutex
	closing            bool
	closed             bool
	runCancel          context.CancelFunc
	runDone            chan struct{}
	backgroundDone     chan struct{}
	serverDone         chan struct{}
	requests           *httpRequests
	httpDrainTimeout   time.Duration
	shutdownJoinBudget time.Duration
	logger             *slog.Logger
	database           *relational.Database
	server             *http.Server
	audits             *auditapp.Service
	auditJournal       *relational.AuditJournal
	historyRetention   *historyapp.Retention
	runtime            io.Closer
	qualityTunables    *qualitymanagement.Service
	settingsBus        repository.SettingsChangeBus
	invalidationBus    repository.InvalidationBus
	settings           *settingsapp.Service
	gateway            *gateway.Service
	media              *mediaapp.Service
	updateCheck        bool
	quotaRecovery      *quotarecoveryapp.Service
	accounts           *accountapp.Service
	models             *modelapp.Service
	clientKeys         *clientkeyapp.Service
	updates            *updatecheckapp.Service
	invalidations      *invalidationapp.Service
	accountRepo        repository.AccountRepository
	modelRepo          repository.ModelRepository
	providers          provider.Registry
	web                *webprovider.Adapter
	egress             *infraegress.Manager
	egressOps          *egressapp.Service
	startup            *startupState
	// Quality owns durable evidence and restrictions; request receipts reach
	// it through the leased incident outbox.
	quality         *qualityregistry.Registry
	qualityEvidence *qualityevidence.Store
	// qualityCourt/qualityInvestigator 是有限仲裁闭环与调查任务队列。
	qualityCourt        *qualitycourt.Service
	qualityInvestigator *qualityinvestigator.Service
	// qualityEnforcement 执行所(重写批4):IP-epoch 检测循环+台账+
	// 人工解禁/批量轮换(API 入口在批5)。
	qualityEnforcement *qualityenforcement.Service
	// qualityProbeExec 调查局真实探针执行器(批6 第4步):
	// 差分/陪审员取证经网关质量探针的 bypass 通道。
	qualityProbeExec qualityinvestigator.Executor
	qualityEvents    *events.Service
	qualityJournal   *journal.Store
	qualityGuard     *qualityguard.Service
}

// installCourtReleaseNotification 把法院的"账号已被本案释放"事实接到账号轴的
// 质量解除上。法院只报告事实,清除质量标记与瞬态冷却是账号应用层的规则。
// 单独成函数,使测试能在不重建整个组合根的前提下锚定同一条接线。
func installCourtReleaseNotification(court *qualitycourt.Service, accounts *accountapp.Service) {
	if court == nil || accounts == nil {
		return
	}
	court.SetAccountReleased(func(ctx context.Context, accountID uint64) error {
		return accounts.ClearQualityHold(ctx, accountID)
	})
}

// New 完成数据库、Provider、应用服务和 HTTP 路由装配。
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (_ *Application, resultErr error) {
	owned := &Application{logger: logger}
	constructed := false
	defer func() {
		if !constructed {
			resultErr = errors.Join(resultErr, owned.Close())
		}
	}()
	var database *relational.Database
	var err error
	switch cfg.Database.Driver {
	case "sqlite":
		database, err = relational.OpenSQLite(ctx, cfg.Database.SQLite.Path)
	case "postgres":
		database, err = relational.OpenPostgres(ctx, cfg.Database.Postgres.DSN, cfg.Database.Postgres.MaxOpenConns, cfg.Database.Postgres.MaxIdleConns)
	default:
		return nil, fmt.Errorf("不支持的数据库驱动: %s", cfg.Database.Driver)
	}
	if err != nil {
		return nil, err
	}
	owned.database = database
	if err := database.InitializeSchema(ctx); err != nil {
		return nil, err
	}
	// Open the quality state pool and reconstruct evidence/identity projections.
	qualityRegistry, qualityEvidenceStore, err := bootstrapQualityLayer(ctx, cfg, logger, relational.NewAccountRepository(database))
	if err != nil {
		return nil, err
	}
	owned.quality = qualityRegistry
	logger.Info("quality_registry_ready",
		"accounts_tracked", qualityRegistry.AccountsTracked(),
		"exits_tracked", qualityRegistry.ExitsTracked())
	// VersionedCipher：轮换 credentialEncryptionKey 后把旧密钥配置进
	// secrets.legacyCredentialEncryptionKeys，存量凭据经回退继续可解。
	cipher, err := security.NewVersionedCipher(cfg.Secrets.CredentialEncryptionKey, cfg.Secrets.LegacyEncryptionKeys)
	if err != nil {
		return nil, err
	}

	adminRepo := relational.NewAdminRepository(database)
	sessionRepo := relational.NewAdminSessionRepository(database)
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	clientKeyRepo := relational.NewClientKeyRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	dashboardRepo := relational.NewDashboardRepository(database)
	runtimeSettingsRepo := relational.NewRuntimeSettingsRepository(database, cipher)
	egressRepo := relational.NewEgressRepository(database)
	mediaJobRepo := relational.NewMediaJobRepository(database)
	mediaAssetRepo := relational.NewMediaAssetRepository(database)
	mediaUploadTicketRepo := relational.NewMediaUploadTicketRepository(database)
	// 文件基线在持久化覆盖前留存，供设置「恢复文件默认」使用。
	fileCfg := cfg
	if err := settingsapp.MigrateLegacyQualityRotation(ctx, config.ToRuntimeSettings(cfg), runtimeSettingsRepo); err != nil {
		return nil, fmt.Errorf("migrate quality rotation capacity: %w", err)
	}
	loadedConfig, settingsUpdatedAt, settingsRevision, err := loadRuntimeSettings(ctx, cfg, runtimeSettingsRepo)
	if err != nil {
		return nil, err
	}
	cfg = loadedConfig
	identity := sha256.Sum256([]byte(cfg.Deployment.ClusterID + "\x00" + cfg.Deployment.InstanceID))
	auditJournal, err := relational.OpenAuditJournal(ctx, filepath.Join(cfg.Audit.JournalDirectory, fmt.Sprintf("pending-%x.db", identity[:16])), relational.AuditJournalOptions{MaxRecords: cfg.Audit.BufferSize, MaxBytes: cfg.Audit.JournalMaxBytes})
	if err != nil {
		return nil, fmt.Errorf("open audit journal: %w", err)
	}
	owned.auditJournal = auditJournal
	localMediaStore, err := inframedia.NewLocalStore(cfg.Media.Local.Path)
	if err != nil {
		return nil, err
	}
	if err := preflightDeployment(cfg); err != nil {
		return nil, err
	}
	var rateLimiter repository.RateLimiter
	var concurrency repository.ConcurrencyLimiter
	var sticky repository.StickySessionRepository
	var reasoningReplayStore repository.ReasoningReplayRepository
	var deviceSessions repository.DeviceSessionRepository
	var refreshLock repository.DistributedLock
	var settingsBus repository.SettingsChangeBus
	var quotaQueue repository.QuotaRecoveryQueue
	var quotaRefreshState repository.QuotaRefreshCoordinator
	var observedModelStore repository.ObservedModelStateRepository
	var invalidationBus repository.InvalidationBus
	var runtimeStore io.Closer
	runtimeHealth := func(context.Context) error { return nil }
	switch cfg.RuntimeStore.Driver {
	case "redis":
		redisStore, openErr := redisruntime.Open(ctx, redisruntime.Config{
			Address: cfg.RuntimeStore.Redis.Address, Username: cfg.RuntimeStore.Redis.Username,
			Password: cfg.RuntimeStore.Redis.Password, Database: cfg.RuntimeStore.Redis.Database,
			KeyPrefix: cfg.RuntimeStore.Redis.KeyPrefix, TLS: cfg.RuntimeStore.Redis.TLS,
			ConcurrencyLease: cfg.Server.RequestTimeout.Value() + time.Minute,
		})
		if openErr != nil {
			return nil, openErr
		}
		runtimeStore = redisStore
		owned.runtime = redisStore
		invalidationBus = redisStore
		runtimeHealth = redisStore.Ping
		rateLimiter = redisStore
		concurrency = redisruntime.NewConcurrencyLimiter(redisStore)
		sticky = redisStore
		reasoningReplayStore = redisruntime.NewReasoningReplayStore(redisStore)
		deviceSessions = redisruntime.NewDeviceSessionStore(redisStore)
		refreshLock = redisruntime.NewLockStore(redisStore)
		settingsBus = redisStore
		quotaQueue = redisStore
		quotaRefreshState = redisStore
		observedModelStore = redisStore
	case "memory":
		rateLimiter = memory.NewRateLimiter()
		concurrency = memory.NewConcurrencyLimiter()
		sticky = memory.NewStickyStore()
		reasoningReplayStore = memory.NewReasoningReplayStore(cfg.Routing.ReasoningReplayMaxEntries)
		deviceSessions = memory.NewDeviceSessionStore()
		refreshLock = memory.NewLockStore()
		quotaQueue = memory.NewQuotaRecoveryQueue()
		quotaRefreshState = memory.NewQuotaRefreshCoordinator()
	default:
		return nil, fmt.Errorf("不支持的运行态驱动: %s", cfg.RuntimeStore.Driver)
	}
	logger.Info("deployment_topology", "replicas", cfg.Deployment.Replicas, "instance_id", cfg.Deployment.InstanceID, "cluster_id", cfg.Deployment.ClusterID, "database", cfg.Database.Driver, "runtime_store", cfg.RuntimeStore.Driver, "media_driver", cfg.Media.Driver, "shared_media", cfg.Deployment.SharedMedia)
	if cfg.Deployment.Replicas > 1 {
		logger.Info("deployment_topology_shared_quality", "replicas", cfg.Deployment.Replicas, "state", "database_authoritative", "probe_limit", "shared_runtime")
	}
	mediaService := mediaapp.NewServiceWithTickets(mediaAssetRepo, mediaJobRepo, mediaUploadTicketRepo, localMediaStore, refreshLock, mediaConfig(cfg))
	mediaImporter := mediaapp.NewImageInputImporter(mediaService, mediafetch.NewImageSource())

	egressManager := infraegress.NewManagerWithLimits(egressRepo, cipher, cfg.Egress.Runtime.LimitsValue())
	owned.egress = egressManager
	egressManager.SetLogger(logger)
	egressManager.SetClearanceLock(refreshLock)
	egressManager.UpdateClearanceConfig(clearanceConfig(cfg))
	egressManager.UpdateBuildTransportSettings(cfg.Provider.Build.ResponseHeaderTimeout.Value(), cfg.Provider.Build.SessionIdleConnTimeout.Value())
	egressManager.UpdateBuildStreamIdleTimeout(cfg.Provider.Build.StreamIdleTimeout.Value())
	cliAdapter := cliprovider.NewAdapter(cliprovider.Config{
		BaseURL: cfg.Provider.Build.BaseURL, FallbackBaseURL: settingsdomain.NormalizeBuildFallbackBaseURL(cfg.Provider.Build.FallbackBaseURL),
		ClientVersion: cfg.Provider.Build.ClientVersion, ClientIdentifier: cfg.Provider.Build.ClientIdentifier,
		TokenAuth: cfg.Provider.Build.TokenAuth, UserAgent: cfg.Provider.Build.UserAgent,
		ResponseHeaderTimeout: cfg.Provider.Build.ResponseHeaderTimeout.Value(),
		StreamIdleTimeout:     cfg.Provider.Build.StreamIdleTimeout.Value(),
	}, cipher)
	cliAdapter.SetLogger(logger)
	// Manager owns Build route selection and transport. The adapter records
	// already-selected paths for the quality management distribution view.
	qualityDialerPolicy := qualityproxy.NewDialerPolicy()
	cliAdapter.SetEgress(qualityProxyDialer{manager: egressManager, policy: qualityDialerPolicy})
	cliAdapter.SetVideoUploadIssuer(mediaService)
	reasoningReplay := historyapp.New(reasoningReplayStore, historyapp.Config{
		Enabled: cfg.Routing.ReasoningReplayEnabled,
		TTL:     cfg.Routing.ReasoningReplayTTL.Value(),
	}, logger)
	conversationJournal := relational.NewConversationJournal(database, cipher, cfg.Routing.ConversationHistoryMaxBytes).WithHotCache(reasoningReplayStore, cfg.Routing.ReasoningReplayTTL.Value())
	legacyReplayAccounts, err := conversationJournal.LegacyBuildAccountIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取历史账号迁移索引: %w", err)
	}
	cliAdapter.SetLegacyReplayAccounts(legacyReplayAccounts)
	reasoningReplay.UseJournal(conversationJournal, cfg.Routing.ConversationHistoryRetention.Value(), 30*time.Minute)
	cliAdapter.SetReasoningReplay(reasoningReplay)
	webAdapter := webprovider.NewAdapter(webProviderConfig(cfg), egressManager, cipher, historyapp.NewResponseResources(responseRepo), mediaService)
	webAdapter.SetLogger(logger)
	consoleAdapter := consoleprovider.NewAdapter(consoleProviderConfig(cfg), egressManager, cipher, mediaService)
	providers := providerimpl.NewRegistry(cliAdapter, webAdapter, consoleAdapter)
	if err := providers.Validate(); err != nil {
		return nil, fmt.Errorf("校验 Provider 注册表: %w", err)
	}
	adminService := adminauth.NewService(adminRepo, sessionRepo, security.NewTokenService(cfg.Secrets.JWTSecret), security.NewBCryptPasswordHasher(), security.RandomTokenSource{}, cfg.Auth.AccessTokenTTL.Value(), cfg.Auth.RefreshTokenTTL.Value())
	adminService.SetLoginRateLimiter(rateLimiter)
	if err := adminService.Bootstrap(ctx, cfg.BootstrapAdmin.Username, cfg.BootstrapAdmin.Password); err != nil {
		return nil, err
	}
	bulkPool := batch.NewSharedPool(maxBatchConcurrency(cfg.Batch), concurrency, "bulk:upstream")
	accountLayerResult, err := bootstrapAccountLayer(ctx, accountLayerDeps{cfg: cfg, logger: logger, accountRepo: accountRepo, modelRepo: modelRepo, auditRepo: auditRepo, deviceSessions: deviceSessions, sticky: sticky, providers: providers, cipher: cipher, refreshLock: refreshLock, concurrency: concurrency, bulkPool: bulkPool, quotaQueue: quotaQueue, quotaRefreshState: quotaRefreshState, observedModelStore: observedModelStore, cliAdapter: cliAdapter})
	if err != nil {
		return nil, err
	}
	importPool, conversionPool, syncPool, refreshPool, detectPool := accountLayerResult.importPool, accountLayerResult.conversionPool, accountLayerResult.syncPool, accountLayerResult.refreshPool, accountLayerResult.detectPool
	accountService, modelService, accountSyncService := accountLayerResult.accountService, accountLayerResult.modelService, accountLayerResult.accountSync
	egressService := egressapp.NewService(egressRepo, cipher)
	owned.egressOps = egressService
	egressService.SetWebhookExecutor(infraegress.NewRotationWebhookExecutor(egressManager))
	egressService.SetSubscriptionFetcher(infraegress.NewSubscriptionFetcher(egressManager, egressapp.NormalizeSubscriptionURL))
	rollingLimiter, rollingOK := rateLimiter.(repository.RollingRateLimiter)
	if !rollingOK {
		return nil, fmt.Errorf("限流实现缺少滚动窗口能力: %T", rateLimiter)
	}
	egressService.SetRotationCoordination(refreshLock, rollingLimiter)
	egressService.SetClearanceManager(egressManager)
	egressService.SetNodeProber(egressManager)
	egressService.SetOperationsConfigInvalidator(egressManager)
	egressService.SetPoolCacheInvalidator(egressManager)
	egressManager.SetFailureProber(egressService.TestNode)
	clientKeyService := clientkeyapp.NewService(fmt.Sprintf("%x", identity[:16]), clientKeyRepo, rateLimiter, concurrency, cfg.ClientKeyDefaults.RPMLimit, cfg.ClientKeyDefaults.MaxConcurrent, cipher, security.RandomTokenSource{})
	owned.clientKeys = clientKeyService
	// 媒体作业预检/清理：删除 key 前处置 media_jobs 的 RESTRICT 外键引用
	// qualityQuarantiner 现仅承载死出口传输冷却(probe_dead),质量隔离态已删。
	egressService.SetQualityQuarantiner(egressManager)
	egressService.SetQualityLogger(logger)
	egressService.SetRotationConfig(egressRotationConfig(cfg))
	egressService.SetRotationLogger(logger)
	//（round 51：失败视频作业曾使 key 不可删并落裸 500）。
	auditService := auditapp.NewService(auditRepo, auditJournal, logger, cfg.Audit.BatchSize, cfg.Audit.FlushInterval.Value())
	owned.audits = auditService
	auditService.UpdateWriterConfig(cfg.Audit.BatchSize, cfg.Audit.FlushInterval.Value(), cfg.Audit.CommitDelay.Value())
	auditService.UpdateLedgerConfig(auditLedgerConfig(cfg.Audit))
	auditService.SetBillingObserver(clientKeyService)
	dashboardService := dashboardapp.NewService(dashboardRepo)
	selector := selector.NewSelector(accountRepo, concurrency, sticky, providers, cfg.Routing.StickyTTL.Value(), cfg.Routing.CooldownBase.Value(), cfg.Routing.CooldownMax.Value(), cfg.Routing.CapacityWait.Value())
	selector.SetLogger(logger)
	selector.UpdatePreferFreeBuild(cfg.Routing.PreferFreeBuild)
	selector.UpdateSegmentedSelector(cfg.Routing.SegmentedSelectorEnabled, cfg.Routing.SegmentedMinCandidates, cfg.Routing.SegmentedWindowSize)
	selector.UpdateExcludeBuildBotFlaggedFromScheduling(cfg.Accounts.ExcludeBuildBotFlaggedFromScheduling)
	accountService.UpdateExcludeBuildBotFlaggedFromScheduling(cfg.Accounts.ExcludeBuildBotFlaggedFromScheduling)
	egressManager.UpdateAccountIsolatedConnections(cfg.Routing.AccountIsolatedConnections)
	invalidationService := invalidationapp.NewService(invalidationBus, invalidationSourceInstance(cfg), func(event repository.InvalidationEvent) {
		selector.ApplyInvalidation(event)
		clientKeyService.ApplyInvalidation(event)
	}, logger)
	accountRepo.SetInvalidationObserver(invalidationService.Notify)
	if err := accountRepo.MigrateLegacyQualityHolds(ctx, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("migrate guard restrictions: %w", err)
	}
	modelRepo.SetInvalidationObserver(invalidationService.Notify)
	clientKeyRepo.SetInvalidationObserver(invalidationService.Notify)
	gatewayService := gateway.NewService(modelService, auditService, accountService, clientKeyService, providers, selector, historyapp.NewResponseResources(responseRepo), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), providerimpl.NewRateLimitInterpreter(), cfg.Routing.MaxAttempts)
	// This synthetic scanner diagnostic has no instance configuration. Actual
	// requests receive the kernel and policy from the guard snapshot below.
	if err := gateway.GuardSelfCheck(); err != nil {
		logger.Error("guard_self_check_failed", "error", err.Error())
	} else {
		logger.Info("guard_self_check", "outcome", "ok")
	}
	// Quality supplies eligibility facts; final admission checks persistent
	// restrictions. Request receipts enter its durable Journal through events.
	qualityJournal := journal.New(qualityRegistry.DB())
	gatewayService.SetAccountQualityEligibility(qualityAccountEligibility{registry: qualityRegistry, journal: qualityJournal})
	egressManager.SetExitEligibility(qualityExitEligibility{registry: qualityRegistry})
	// 审判系:有限仲裁评估+调查局任务队列。立案即冻结状态并派发
	// 一个共享资源证明任务；旧案保留其冻结的历史协议。
	qualityCourtService, qualityInvestigatorService, qualityProbeStore := bootstrapJudicialLayer(qualityRegistry, qualityEvidenceStore, logger)
	owned.qualityCourt = qualityCourtService
	qualityCourtService.SetProbeAccounts(gatewayService)
	qualityCourtService.SetNodes(baseNodeSource{egress: egressService})
	// 被告存活缝:账号删除后法院销案,杜绝差分探针对不存在账号的
	// 无限重派(批9 事故根因:load account 永远失败被统一术语掩盖)。
	qualityCourtService.SetAccountExists(func(ctx context.Context, accountID uint64) bool {
		_, err := accountService.Get(ctx, accountID)
		return !errors.Is(err, accountapp.ErrNotFound)
	})
	qualityCourtService.SetProofIdentityCheck(func(ctx context.Context, sample qualitymodel.ResourceSample) (bool, error) {
		credential, err := accountRepo.Get(ctx, sample.Attempt.AccountID)
		if errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if credential.CredentialGeneration != sample.CredentialGeneration || string(credential.Provider) != sample.Attempt.Provider || credential.BuildRouteMode == accountdomain.BuildRouteXAI {
			return false, nil
		}
		node, err := egressRepo.GetEgressNode(ctx, sample.Attempt.Path.NodeID)
		if errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return node.BindingRevision == sample.PathBinding, nil
	})
	// 同出口排除集(建议性前置过滤):差分比对候选若已知与 baseline 共享
	// 真实出口,在派发前跳过,避免白耗一次探针并少一条可采证据。判定只是
	// 预筛——gateway.verifySecondPath 的活体出口核实仍是可采性的唯一权威。
	qualityCourtService.SetSameExit(managerExitIPResolver{Manager: egressManager}.KnownSameExit)
	// 无罪即恢复:法院确认出口有过错、账号被洗清后,账号轴的质量标记与随之而来
	// 的瞬态冷却立即解除,不必再等它自然到期。
	installCourtReleaseNotification(qualityCourtService, accountService)
	qualityEvents := events.New(qualityJournal, qualityEvidenceStore, qualityCourtService)
	gatewayService.SetQualityEventRecorder(&qualityEventSink{Service: qualityEvents})
	// 执行所(重写批4):epoch 检测+台账;台账接仲裁庭立案(G8)。
	qualityEnforcementService := bootstrapEnforcementLayer(qualityRegistry, egressService)
	owned.qualityEnforcement = qualityEnforcementService
	qualityCourtService.SetLedgerSink(qualityEnforcementService)
	// 轮换成功 → 立即驱动单节点出口身份观测:webhook 已验证身份变化,
	// 解禁不必等下一个检测节拍(尾延≤5m → 秒级)。观测回调只消费已
	// 落库的探活事实,失败仅记日志,不影响轮换结果。
	egressService.SetRotationSuccessObserver(func(ctx context.Context, nodeID uint64) {
		if _, _, err := qualityEnforcementService.ObserveNodeExit(ctx, nodeID); err != nil {
			logger.Warn("quality_rotation_observe_failed", "node", nodeID, "error", err.Error())
		}
	})
	// 调查局真实探针执行器:网关质量探针 + 出口 IP 取证面。
	gatewayService.SetNodeExitIPResolver(managerExitIPResolver{Manager: egressManager})
	qualityProbeExecutorService := qualityinvestigator.NewProbeExecutor(qualityRegistry, gatewayService, logger)
	// 守卫配置面(重写批5:G13 管辖勾选+I4 自检可见)+探针队列视图。
	// requestRetry 的文件启停/预算是底座基线;管辖清单若有显式条目则
	// 继承，否则沿用质量守卫的默认主力模型清单，避免空切片被误当成
	// “无管辖”而让服务启动后静默失效。
	qualityGuardService := bootstrapGuardService(ctx, relational.NewSettingsDocumentRepository(database, qualityguard.SettingsKey), logger,
		qualityGuardConfig(cfg.RequestRetry), qualityGuardConfig(fileCfg.RequestRetry))
	guardSource := qualityGuardSnapshotSource{service: qualityGuardService, registry: qualityRegistry}
	gatewayService.SetGuardSnapshotSource(guardSource)
	installedGuard, guardErr := qualityGuardService.Snapshot()
	guardOrigin := "bootstrap"
	if installedGuard.Revision > 0 {
		guardOrigin = "quality_guard"
	}
	logger.Info("quality_guard_effective", "source", guardOrigin,
		"revision", installedGuard.Revision, "ready", guardErr == nil,
		"enabled", installedGuard.Enabled, "max_attempts", installedGuard.MaxAttempts,
		"on_exhausted", installedGuard.ExhaustionPolicy(), "guarded_models", strings.Join(installedGuard.GuardedModels, ","))

	gatewayService.UpdateVideoMaxAttempts(cfg.Routing.VideoMaxAttempts)
	gatewayService.UpdateMarkBuildChatDeniedAsReauth(cfg.Routing.MarkBuildChatDeniedAsReauth)
	gatewayService.SetLogger(logger)
	gatewayService.UpdateBuildForbiddenReauthPolicy(cfg.Accounts.MarkBuildForbiddenReauth, cfg.Accounts.BuildForbiddenReauthCodes)
	gatewayService.UpdateRequestTimeout(cfg.Server.RequestTimeout.Value())
	gatewayService.ConfigureMedia(mediaJobRepo, mediaapp.NewVideoResources(mediaJobRepo, mediaService), cfg.Provider.Web.MediaConcurrency)
	quotaRecoveryService := quotarecoveryapp.NewService(logger, quotaQueue, accountService, cfg.Provider.Web.RecoveryBackoffBase.Value(), cfg.Provider.Web.RecoveryBackoffMax.Value())
	quotaRecoveryService.SetBulkPool(syncPool)
	inferenceConcurrency := httpmiddleware.NewConcurrencyGate(cfg.Server.MaxConcurrentRequests)
	var notifySettings func(context.Context)
	var publishSettings func(context.Context) error
	if settingsBus != nil {
		publishSettings = settingsBus.PublishSettingsChanged
		notifySettings = func(notifyCtx context.Context) {
			publishCtx, cancel := context.WithTimeout(context.WithoutCancel(notifyCtx), 3*time.Second)
			defer cancel()
			if err := settingsBus.PublishSettingsChanged(publishCtx); err != nil {
				logger.Warn("settings_change_publish_failed", "error", err)
			}
		}
	}
	settingsService := settingsapp.NewService(config.ToRuntimeSettings(cfg), settingsUpdatedAt, settingsRevision, runtimeSettingsRepo, publishSettings, []settingsapp.ApplyTarget{
		{Name: "inference_capacity", Apply: settingsApply(fileCfg, func(next config.Config) error {
			inferenceConcurrency.UpdateLimit(next.Server.MaxConcurrentRequests)
			return nil
		})},
		{Name: "batch", Apply: settingsApply(fileCfg, func(next config.Config) error {
			bulkPool.UpdateLimit(maxBatchConcurrency(next.Batch))
			importPool.UpdateLimit(next.Batch.ImportConcurrency)
			conversionPool.UpdateLimit(next.Batch.ConversionConcurrency)
			syncPool.UpdateLimit(next.Batch.SyncConcurrency)
			refreshPool.UpdateLimit(next.Batch.RefreshConcurrency)
			detectPool.UpdateLimit(32)
			for _, pool := range []*batch.Pool{importPool, conversionPool, syncPool, refreshPool, detectPool} {
				pool.UpdateJitter(next.Batch.RandomDelay.Value())
			}
			return nil
		})},
		{Name: "provider_build", Apply: settingsApply(fileCfg, func(next config.Config) error {
			cliAdapter.UpdateConfig(cliprovider.Config{
				BaseURL: next.Provider.Build.BaseURL, FallbackBaseURL: settingsdomain.NormalizeBuildFallbackBaseURL(next.Provider.Build.FallbackBaseURL),
				ClientVersion: next.Provider.Build.ClientVersion, ClientIdentifier: next.Provider.Build.ClientIdentifier,
				TokenAuth: next.Provider.Build.TokenAuth, UserAgent: next.Provider.Build.UserAgent,
				ResponseHeaderTimeout: next.Provider.Build.ResponseHeaderTimeout.Value(),
				StreamIdleTimeout:     next.Provider.Build.StreamIdleTimeout.Value(),
			})
			return nil
		})},
		{Name: "network", Apply: settingsApply(fileCfg, func(next config.Config) error {
			egressManager.UpdateBuildTransportSettings(next.Provider.Build.ResponseHeaderTimeout.Value(), next.Provider.Build.SessionIdleConnTimeout.Value())
			egressManager.UpdateBuildStreamIdleTimeout(next.Provider.Build.StreamIdleTimeout.Value())
			egressManager.UpdateClearanceConfig(clearanceConfig(next))
			egressManager.UpdateAccountIsolatedConnections(next.Routing.AccountIsolatedConnections)
			return nil
		})},
		{Name: "provider_web", Apply: settingsApply(fileCfg, func(next config.Config) error {
			webAdapter.UpdateConfig(webProviderConfig(next))
			return nil
		})},
		{Name: "provider_console", Apply: settingsApply(fileCfg, func(next config.Config) error {
			consoleAdapter.UpdateConfig(consoleProviderConfig(next))
			return nil
		})},
		{Name: "media", Apply: settingsApply(fileCfg, func(next config.Config) error {
			mediaService.UpdateConfig(mediaConfig(next))
			return nil
		})},
		{Name: "quota_recovery", Apply: settingsApply(fileCfg, func(next config.Config) error {
			quotaRecoveryService.UpdateConfig(next.Provider.Web.RecoveryBackoffBase.Value(), next.Provider.Web.RecoveryBackoffMax.Value())
			return nil
		})},
		{Name: "account_sync", Apply: settingsApply(fileCfg, func(next config.Config) error {
			accountSyncService.UpdateConcurrency(next.Batch.ImportConcurrency)
			return nil
		})},
		{Name: "selector", Apply: settingsApply(fileCfg, func(next config.Config) error {
			selector.UpdateConfig(next.Routing.StickyTTL.Value(), next.Routing.CooldownBase.Value(), next.Routing.CooldownMax.Value(), next.Routing.CapacityWait.Value())
			selector.UpdatePreferFreeBuild(next.Routing.PreferFreeBuild)
			selector.UpdateSegmentedSelector(next.Routing.SegmentedSelectorEnabled, next.Routing.SegmentedMinCandidates, next.Routing.SegmentedWindowSize)
			selector.UpdateExcludeBuildBotFlaggedFromScheduling(next.Accounts.ExcludeBuildBotFlaggedFromScheduling)
			return nil
		})},
		{Name: "egress_rotation", Apply: settingsApply(fileCfg, func(next config.Config) error {
			egressService.SetRotationConfig(egressRotationConfig(next))
			return nil
		})},
		{Name: "accounts", Apply: settingsApply(fileCfg, func(next config.Config) error {
			accountService.UpdateExcludeBuildBotFlaggedFromScheduling(next.Accounts.ExcludeBuildBotFlaggedFromScheduling)
			accountService.UpdateAutoCleanConfig(accountAutoCleanConfig(next.Accounts))
			return nil
		})},
		{Name: "conversation", Apply: settingsApply(fileCfg, func(next config.Config) error {
			reasoningReplay.UpdateConfig(historyapp.Config{Enabled: next.Routing.ReasoningReplayEnabled, TTL: next.Routing.ReasoningReplayTTL.Value()})
			return nil
		})},
		{Name: "gateway", Apply: settingsApply(fileCfg, func(next config.Config) error {
			gatewayService.UpdateMaxAttempts(next.Routing.MaxAttempts)
			gatewayService.UpdateVideoMaxAttempts(next.Routing.VideoMaxAttempts)
			gatewayService.UpdateMarkBuildChatDeniedAsReauth(next.Routing.MarkBuildChatDeniedAsReauth)
			gatewayService.UpdateBuildForbiddenReauthPolicy(next.Accounts.MarkBuildForbiddenReauth, next.Accounts.BuildForbiddenReauthCodes)
			return nil
		})},
		{Name: "audit", Apply: settingsApply(fileCfg, func(next config.Config) error {
			auditService.UpdateWriterConfig(next.Audit.BatchSize, next.Audit.FlushInterval.Value(), next.Audit.CommitDelay.Value())
			auditService.UpdateLedgerConfig(auditLedgerConfig(next.Audit))
			return nil
		})},
		{Name: "client_keys", Apply: settingsApply(fileCfg, func(next config.Config) error {
			clientKeyService.UpdateDefaults(next.ClientKeyDefaults.RPMLimit, next.ClientKeyDefaults.MaxConcurrent)
			return nil
		})},
	})
	settingsService.SetRequestRetryProjection(func() settingsdomain.RequestRetryConfig {
		current := qualityGuardService.Config()
		return settingsdomain.RequestRetryConfig{Enabled: current.Enabled, MaxAttempts: current.MaxAttempts,
			OnExhausted: "fail_closed", GuardedModels: current.GuardedModels,
			EvidenceTimeout: current.EvidenceTimeout, CreatedTimeout: current.CreatedTimeout,
			AccountCooldown: current.AccountCooldown, IdleAccountCooldown: current.IdleAccountCooldown}
	})
	settingsService.SetRuntimeValidator(func(runtime settingsdomain.Config) error {
		_, err := config.ApplyRuntimeSnapshot(fileCfg, runtime)
		return err
	})
	settingsService.SetPersistedResolver(func(runtime settingsdomain.Config) (settingsdomain.Config, error) {
		return config.ResolveRuntimeSettings(fileCfg, runtime)
	})
	settingsService.SetFileConfig(config.ToRuntimeSettings(fileCfg))
	updateService := updatecheckapp.NewService(buildinfo.CurrentVersion(), updatecheckinfra.NewGitHubSource(nil))
	// Quality owns parameter policy; composition supplies persistence, apply and notification ports.
	qualityTunables := qualitymanagement.New(
		relational.NewSettingsDocumentRepository(database, qualitymanagement.SettingsKey),
		(qualitymanagement.Runtime{Court: qualityCourtService, Investigator: qualityInvestigatorService, Evidence: qualityEvidenceStore}).Apply,
		notifySettings,
	)
	if err := qualityTunables.ReloadPersisted(ctx); err != nil {
		logger.Error("quality_tunables_startup_pending", "error", err)
	}

	qualityQueries := qualitymanagement.NewQueries(qualitymanagement.QueryDependencies{
		Registry: qualityRegistry, Probes: qualityProbeStore, Court: qualityCourtService,
		Evidence: qualityEvidenceStore, Guard: qualityGuardService,
		Nodes: baseNodeSource{egress: egressService},
		// Preserve the old DTO field; production receipts use the durable queue.
		ObservationDrops: func() int64 { return 0 },
	})

	startup := newStartupState(accountLayerResult.quotaRecoveries)
	readiness := func(readyCtx context.Context) httpserver.ReadinessSnapshot {
		return readinessSnapshot(readyCtx, startup, runtimeHealth, modelRepo, accountRepo, providers, auditService)
	}
	accountService.SetQualityStates(accountQualityStatesProvider{registry: qualityRegistry}.states)
	qualityProbeExecutorService.SetResourceChecks(gateway.ResourceCheckMeasurer{Gateway: gatewayService, Paths: resourceCheckPaths{manager: egressManager, settings: settingsService}}, qualityProbeStore)
	router := httpserver.New(httpserver.Dependencies{Logger: logger, RequestTokens: security.RandomTokenSource{}, RequestTimeout: cfg.Server.RequestTimeout.Value(), MaxBodyBytes: cfg.Server.MaxBodyBytes, TrustedProxies: cfg.Server.TrustedProxies, ConcurrencyGate: inferenceConcurrency, SecureCookies: cfg.Auth.SecureCookies, SwaggerEnabled: cfg.Server.SwaggerEnabled, PublicAPIBaseURL: cfg.Frontend.EffectivePublicAPIBaseURL(), FrontendStaticPath: cfg.Frontend.StaticPath, Readiness: readiness, TrafficReady: startup.acceptsTraffic, AdminAuth: adminService, Accounts: accounthttp.Dependencies{Administration: accountService, Credentials: accountService, Maintenance: accountService, Onboarding: accountsyncapp.NewOnboarding(accountService, accountService, accountSyncService), DeviceOnboarding: accountSyncService, ModelSyncUpdates: accountSyncService}, Models: modelService, ClientKeys: clientKeyService, ClientAuthKeys: clientKeyService, Audits: auditService, Dashboard: dashboardService, Gateway: gatewayService, Media: mediaService, MediaImporter: mediaImporter, Settings: settingsService, Egress: egressService, EgressLiveStats: liveEgressStats(egressManager), Updates: updateService, Quality: &qualityhttp.Deps{ResourceChecks: qualitymanagement.NewResourceChecks(qualityProbeStore, gatewayService, baseNodeSource{egress: egressService}), Queries: qualityQueries, Court: qualityCourtService, Enforcement: qualityEnforcementService, Guard: qualityGuardService, DialerDistribution: qualityDialerPolicy.SelectionDistribution, Tunables: qualityTunables, RotationCapacity: func() int { return egressService.RotationConfig().MaxGlobalPerHour }}, EgressQualityStates: egressQualityStatesProvider{registry: qualityRegistry}.states})
	logSecureCookiesHint(logger, cfg)
	server := &http.Server{Addr: cfg.Server.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: cfg.Server.ReadTimeout.Value(), IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	constructed = true
	return &Application{
		logger: logger, database: database, server: server,
		audits: auditService, auditJournal: auditJournal, historyRetention: historyapp.NewRetention(responseRepo, conversationJournal, refreshLock, logger), runtime: runtimeStore,
		qualityTunables: qualityTunables, settingsBus: settingsBus, invalidationBus: invalidationBus, settings: settingsService, gateway: gatewayService, media: mediaService, quotaRecovery: quotaRecoveryService, accounts: accountService, models: modelService, clientKeys: clientKeyService, updates: updateService, invalidations: invalidationService, updateCheck: serverUpdateCheckEnabled(cfg),
		accountRepo: accountRepo, modelRepo: modelRepo, providers: providers, web: webAdapter, egress: egressManager, egressOps: egressService, startup: startup,
		quality: qualityRegistry, qualityEvidence: qualityEvidenceStore,
		qualityCourt: qualityCourtService, qualityInvestigator: qualityInvestigatorService,
		qualityEnforcement: qualityEnforcementService, qualityProbeExec: qualityProbeExecutorService,
		qualityEvents: qualityEvents, qualityJournal: qualityJournal, qualityGuard: qualityGuardService,
	}, nil
}

// Run 启动 HTTP 服务和本地后台维护任务。
func (a *Application) Run(ctx context.Context) (resultErr error) {
	ctx, err := a.beginRun(ctx)
	if err != nil {
		return err
	}
	defer close(a.runDone)
	defer a.runCancel()
	defer func() { resultErr = errors.Join(resultErr, a.closeClientKeyTouches()) }()
	defer func() { resultErr = errors.Join(resultErr, a.closeModelSync()) }()
	if a.egress != nil {
		if err := a.egress.Start(ctx); err != nil {
			return err
		}
	}
	if err := a.audits.Start(ctx); err != nil {
		return fmt.Errorf("start audit recovery: %w", err)
	}
	runCtx, cancelBackground := context.WithCancel(ctx)
	var background sync.WaitGroup
	a.backgroundDone = make(chan struct{})
	defer func() {
		resultErr = errors.Join(resultErr, a.drainHTTP())
		cancelBackground()
		go func() { background.Wait(); close(a.backgroundDone) }()
		waitCtx, cancel := context.WithTimeout(context.Background(), a.joinTimeout())
		defer cancel()
		if err := waitStopped(waitCtx, a.serverDone); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("wait for HTTP listener: %w", err))
		}
		if err := waitStopped(waitCtx, a.backgroundDone); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("wait for application workers: %w", err))
		}
	}()
	// 出口 IP 轮换 worker 常驻等待事件，支持禁用后热启用。
	background.Add(1)
	go func() {
		defer background.Done()
		a.egressOps.RunRotationWorker(runCtx)
	}()
	errCh := make(chan error, 1)
	servedAt := time.Now()
	a.lifecycleMu.Lock()
	a.requests = newHTTPRequests(a.server)
	a.lifecycleMu.Unlock()
	a.serverDone = make(chan struct{})
	go func() {
		defer close(a.serverDone)
		a.logger.Info("server_started", "listen", a.server.Addr, "version", buildinfo.CurrentVersion(), "commit", buildinfo.CurrentCommit())
		errCh <- a.server.ListenAndServe()
	}()
	a.reconcileStartup(runCtx)
	startBackground := func(name string, task func(context.Context) error) {
		background.Add(1)
		go func() {
			defer background.Done()
			a.runSupervisedTask(runCtx, name, task)
		}()
	}
	if a.invalidationBus != nil {
		startBackground("invalidation_publisher", a.invalidations.RunPublisher)
		startBackground("invalidation_subscriber", a.invalidations.RunSubscriber)
	}
	if a.qualityEvents != nil {
		startBackground("guard_event_outbox", a.qualityEvents.Run)
		startBackground("guard_completion_recovery", a.qualityJournal.RunCompletionRecovery)
		startBackground("guard_event_retention", func(taskCtx context.Context) error {
			a.runPeriodicTask(taskCtx, time.Minute, "guard_event_retention", func(runCtx context.Context) error {
				sweepCtx, cancel := context.WithTimeout(runCtx, 5*time.Second)
				defer cancel()
				return a.qualityJournal.Sweep(sweepCtx, time.Now().UTC())
			})
			return nil
		})
	}
	if a.qualityGuard != nil {
		startBackground("guard_policy_reconcile", func(taskCtx context.Context) error {
			a.runPeriodicTask(taskCtx, 5*time.Second, "guard_policy_reconcile", func(runCtx context.Context) error {
				return a.qualityGuard.LoadPersisted(runCtx)
			})
			return nil
		})
	}
	if a.quality != nil && a.qualityEvidence != nil {
		startBackground("quality_shared_state", func(taskCtx context.Context) error {
			a.runPeriodicTask(taskCtx, 3*time.Second, "quality_shared_state", func(ctx context.Context) error {
				workCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				if err := a.quality.RefreshState(workCtx); err != nil {
					return err
				}
				return a.qualityEvidence.RefreshWindow(workCtx)
			})
			return nil
		})
	}
	startBackground("settings_reconcile", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 30*time.Second, "settings_reconcile", func(runCtx context.Context) error {
			return a.reloadSettings(runCtx)
		})
		return nil
	})
	startBackground("performance_metrics", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, time.Minute, "performance_metrics", func(context.Context) error {
			a.logPerformanceMetrics()
			return nil
		})
		return nil
	})
	if a.updateCheck {
		startBackground("release_check", func(taskCtx context.Context) error {
			a.updates.Check(taskCtx)
			a.runPeriodicTask(taskCtx, 24*time.Hour, "release_check", func(checkCtx context.Context) error {
				a.updates.Check(checkCtx)
				return nil
			})
			return nil
		})
	} else {
		a.logger.Info("update_check_disabled", "reason", "server.updateCheckEnabled=false")
	}
	startBackground("billing_reservation_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 10*time.Minute, "billing_reservation_cleanup", func(runCtx context.Context) error {
			_, err := a.clientKeys.CleanupExpiredBilling(runCtx, 1000)
			return err
		})
		return nil
	})
	startBackground("audit_retention", func(taskCtx context.Context) error {
		return a.audits.RunRetention(taskCtx, a.settings)
	})
	// SQLite freelist 页归还：retention/媒体作业删除产生的空闲页只有
	// 周期 incremental_vacuum 才真正缩小文件（auto_vacuum=INCREMENTAL
	// 仅启用机制）。日频足够（页增速慢），非 SQLite 方言内部 no-op。
	startBackground("sqlite_incremental_vacuum", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 24*time.Hour, "sqlite_incremental_vacuum", func(runCtx context.Context) error {
			trimmed, err := a.database.SQLiteIncrementalVacuum(runCtx)
			if err == nil && trimmed {
				a.logger.Info("sqlite_incremental_vacuum_trimmed", "hint", "freelist pages returned to the filesystem")
			}
			return err
		})
		return nil
	})
	startBackground("model_cooldown_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 10*time.Minute, "model_cooldown_cleanup", func(runCtx context.Context) error {
			_, err := a.accountRepo.PruneExpiredModelQuotaBlocks(runCtx, time.Now().UTC(), 1000)
			return err
		})
		return nil
	})
	startBackground("conversation_history_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, historyapp.ConversationCleanupInterval, "conversation_history_cleanup", func(runCtx context.Context) error {
			return a.historyRetention.CleanupConversations(runCtx, time.Now().UTC())
		})
		return nil
	})
	startBackground("response_ownership_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, historyapp.ResponseCleanupInterval, "response_ownership_cleanup", func(runCtx context.Context) error {
			return a.historyRetention.CleanupResponses(runCtx, time.Now().UTC())
		})
		return nil
	})

	startBackground("quota_recovery", func(taskCtx context.Context) error {
		a.quotaRecovery.Run(taskCtx)
		return nil
	})
	startBackground("quota_refresh", func(taskCtx context.Context) error {
		a.accounts.RunQuotaRefresh(taskCtx)
		return nil
	})
	startBackground("credential_refresh", func(taskCtx context.Context) error {
		a.accounts.RunCredentialRefresh(taskCtx)
		return nil
	})
	startBackground("account_auto_clean", func(taskCtx context.Context) error {
		a.accounts.RunAccountAutoClean(taskCtx)
		return nil
	})
	startBackground("statsig_warmup", func(taskCtx context.Context) error {
		a.runStatsigWarmup(taskCtx)
		return nil
	})
	startBackground("web_quota_startup_catchup", func(taskCtx context.Context) error {
		a.runWebQuotaCatchup(taskCtx)
		return nil
	})
	startBackground("console_usage_migration", func(taskCtx context.Context) error {
		a.runConsoleUsageMigration(taskCtx)
		return nil
	})
	startBackground("console_quota_stale_catchup", func(taskCtx context.Context) error {
		a.runConsoleQuotaCatchup(taskCtx)
		return nil
	})
	startBackground("model_catalog_startup_catchup", func(taskCtx context.Context) error {
		a.runModelCatalogCatchup(taskCtx)
		return nil
	})
	startBackground("video_recovery", func(taskCtx context.Context) error {
		a.gateway.RunVideoRecovery(taskCtx)
		return nil
	})
	startBackground("video_workers", func(taskCtx context.Context) error {
		a.gateway.RunVideoWorkers(taskCtx)
		return nil
	})
	startBackground("media_cleanup", func(taskCtx context.Context) error {
		a.media.RunCleanup(taskCtx, func(err error) {
			if !observability.IsShutdownCancellation(taskCtx, err) {
				a.logger.Warn("media_cleanup_failed", "error", err)
			}
		})
		return nil
	})
	startBackground("clearance_refresh", func(taskCtx context.Context) error {
		if err := a.egress.RefreshDueClearances(taskCtx, false); err != nil {
			a.logger.Warn("clearance_initial_refresh_failed", "error", err)
		}
		a.runPeriodicTask(taskCtx, time.Minute, "clearance_refresh", func(runCtx context.Context) error {
			if err := a.egress.RefreshDueClearances(runCtx, false); err != nil {
				a.logger.Warn("clearance_refresh_failed", "error", err)
			}
			return nil
		})
		return nil
	})
	if a.qualityInvestigator != nil {
		startBackground("quality_probe_projection", a.qualityInvestigator.RunProjections)
	}
	// 质量仲裁与执行所的循环由组合根显式启动(构造不再隐式起循环)。
	// Run 为非阻塞 API(内部自起循环 goroutine 并由 Close 取消),监督器
	// 期待任务阻塞到取消——此处持有任务槽直到 ctx 结束,避免把立即返回
	// 误判为"意外退出"而无限重启。
	if a.qualityCourt != nil {
		startBackground("quality_court_evaluator", func(taskCtx context.Context) error {
			a.qualityCourt.Run(taskCtx)
			<-taskCtx.Done()
			return nil
		})
	}
	if a.qualityEnforcement != nil {
		startBackground("quality_enforcement_loop", func(taskCtx context.Context) error {
			a.qualityEnforcement.Run(taskCtx)
			<-taskCtx.Done()
			return nil
		})
	}
	startBackground("quality_probe_worker", func(taskCtx context.Context) error {
		return a.runQualityProbeWorker(taskCtx)
	})
	// 身份组周期刷新(批2 遗留闭合):账号 CRUD 无缝隙点,周期整表
	// 重算替代事件钩子——q_identity_group 是派生数据(幂等),5m 节拍
	// 下运行期 SSO 关联变化至多延迟一个节拍生效(成员集合不变的
	// 重算为 no-op)。
	if a.quality != nil {
		startBackground("quality_identity_refresh", func(taskCtx context.Context) error {
			a.runPeriodicTask(taskCtx, 5*time.Minute, "quality_identity_refresh", func(runCtx context.Context) error {
				if err := a.quality.RefreshIdentityGroups(runCtx); err != nil && !observability.IsShutdownCancellation(runCtx, err) {
					a.logger.Warn("quality_identity_refresh_failed", "error", err)
				}
				return nil
			})
			return nil
		})
	}
	// 历史保留清扫(批10):案件/探针历史表有界化——
	// 观测表证据局自带清理,此前案件/当事方/探针
	// 明细无清理路径(无界增长)。小时级节拍,
	// 清理量为 0 时不打日志。
	if a.quality != nil {
		startBackground("quality_history_retention", func(taskCtx context.Context) error {
			a.runPeriodicTask(taskCtx, time.Hour, "quality_history_retention", func(runCtx context.Context) error {
				cases, parties, probes, err := a.quality.CleanExpiredCaseHistory(runCtx, time.Now().UTC())
				if err != nil {
					a.logger.Warn("quality_history_retention_failed", "error", err.Error())
				} else if cases > 0 || probes > 0 {
					a.logger.Info("quality_history_retention_swept", "cases", cases, "parties", parties, "probes", probes)
				}
				return nil
			})
			return nil
		})
	}
	for _, pass := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"egress_subscriptions", a.egressOps.RunSubscriptionMaintenance}, {"egress_probes", a.egressOps.RunProbeMaintenance},
	} {
		startBackground(pass.name, func(taskCtx context.Context) error {
			if err := pass.run(taskCtx); err != nil && !observability.IsShutdownCancellation(taskCtx, err) {
				a.logger.Warn(pass.name+"_initial_run_failed", "error", err)
			}
			a.runPeriodicTask(taskCtx, time.Minute, pass.name, pass.run)
			return nil
		})
	}

	if a.settingsBus != nil {
		startBackground("settings_change_listener", func(taskCtx context.Context) error {
			return a.settingsBus.ListenSettingsChanges(taskCtx, func(eventCtx context.Context) error {
				reloadCtx, cancel := context.WithTimeout(eventCtx, 5*time.Second)
				defer cancel()
				if err := a.reloadSettings(reloadCtx); err != nil {
					a.logger.Warn("settings_reload_failed", "error", err)
				}
				return nil
			})
		})
	}
	a.queueDueWebQuotaRefresh(runCtx)
	select {
	case <-ctx.Done():
		a.logger.Info("server_stopping", "uptime_ms", time.Since(servedAt).Milliseconds())
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (a *Application) logPerformanceMetrics() {
	stats := a.database.Stats()
	databaseLabels := perfmetrics.Labels{Subsystem: "database", Operation: a.database.Dialect()}
	perfmetrics.Default.SetGauge("db_open_connections", databaseLabels, int64(stats.OpenConnections))
	perfmetrics.Default.SetGauge("db_in_use_connections", databaseLabels, int64(stats.InUse))
	perfmetrics.Default.SetGauge("db_idle_connections", databaseLabels, int64(stats.Idle))
	perfmetrics.Default.SetGauge("db_wait_count", databaseLabels, stats.WaitCount)
	perfmetrics.Default.SetGauge("db_wait_duration_us", databaseLabels, stats.WaitDuration.Microseconds())
	if a.audits != nil {
		a.audits.LedgerSnapshot()
	}
	if a.accounts != nil {
		quota := a.accounts.QuotaRefreshStats()
		labels := perfmetrics.Labels{Subsystem: "quota", Operation: "refresh"}
		perfmetrics.Default.SetGauge("quota_refresh_pending", labels, int64(quota.Pending))
		perfmetrics.Default.SetGauge("quota_refresh_queued", labels, int64(quota.Queued))
		perfmetrics.Default.SetGauge("quota_refresh_running", labels, int64(quota.Running))
	}
	// 守卫在场性 gauge:外部告警可对"守卫关闭期间仍有流量"建规则(计数为
	// 0/1,多实例各报各的,与 guard-stats 同语义)。
	if effective := a.gateway.GuardStatsSnapshot().Effective; effective != nil {
		enabled := int64(0)
		if effective.Enabled {
			enabled = 1
		}
		perfmetrics.Default.SetGauge("quality_guard_enabled", perfmetrics.Labels{Subsystem: "gateway"}, enabled)
	}
	for _, sample := range perfmetrics.Default.CollectAndReset() {
		a.logger.Info("performance_metric",
			"name", sample.Name,
			"subsystem", sample.Labels.Subsystem,
			"operation", sample.Labels.Operation,
			"provider", sample.Labels.Provider,
			"plane", sample.Labels.Plane,
			"stage", sample.Labels.Stage,
			"ordinal", sample.Labels.Ordinal,
			"outcome", sample.Labels.Outcome,
			"count", sample.Count,
			"total", sample.Total,
			"maximum", sample.Maximum,
			"gauge", sample.Gauge,
			"has_gauge", sample.HasGauge,
		)
	}
}

func (a *Application) Close() error {
	a.closeMu.Lock()
	defer a.closeMu.Unlock()
	if a.closed {
		return nil
	}
	if err := a.stopRun(); err != nil {
		return err
	}
	if err := a.closeDependencies(); err != nil {
		return err
	}
	a.closed = true
	if a.logger != nil {
		a.logger.Info("application_closed")
	}
	return nil
}

func (a *Application) closeDependencies() error {
	// Stop producers before their network, observation, runtime and SQL ports.
	if err := a.closeClientKeyTouches(); err != nil {
		return err
	}
	if err := a.closeModelSync(); err != nil {
		return err
	}
	if a.qualityCourt != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := a.qualityCourt.Close(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("close quality court before dependencies: %w", err)
		}
	}
	if a.qualityEnforcement != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := a.qualityEnforcement.Close(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("close quality enforcement before dependencies: %w", err)
		}
	}
	if a.egressOps != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := a.egressOps.Close(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("close egress maintenance before dependencies: %w", err)
		}
	}
	if a.egress != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := a.egress.Close(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("close egress before storage: %w", err)
		}
	}
	if a.audits != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := a.audits.Close(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("close audit before storage: %w", err)
		}
	}
	var journalErr, runtimeErr, qualityErr, databaseErr error
	if a.auditJournal != nil {
		journalErr = a.auditJournal.Close()
	}
	if a.runtime != nil {
		runtimeErr = a.runtime.Close()
	}
	if a.quality != nil {
		qualityErr = a.quality.Close()
	}
	if a.database != nil {
		databaseErr = a.database.Close()
	}
	return errors.Join(journalErr, runtimeErr, qualityErr, databaseErr)
}

func (a *Application) runPeriodicTask(ctx context.Context, interval time.Duration, name string, task func(context.Context) error) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runCtx, cancel := context.WithTimeout(ctx, minDuration(interval, 5*time.Minute))
			err := task(runCtx)
			cancel()
			if err != nil && !observability.IsShutdownCancellation(ctx, err) {
				a.logger.Warn(name+"_failed", "error", err)
			}
			resetTimer(timer, interval)
		}
	}
}

func (a *Application) runSupervisedTask(ctx context.Context, name string, task func(context.Context) error) {
	backoff := time.Second
	for {
		err := batch.Do(ctx, task)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("后台任务意外退出")
		}
		var panicErr *batch.PanicError
		if errors.As(err, &panicErr) {
			a.logger.Error("background_task_restarting", "task", name, "backoff", backoff, "error", panicErr, "stack", string(panicErr.Stack))
		} else {
			a.logger.Error("background_task_restarting", "task", name, "backoff", backoff, "error", err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

// logSecureCookiesHint 在 secureCookies=true 时提示 HTTP 直连陷阱：浏览器
// 拒收 Secure cookie 表现为登录循环，且服务端此前无任何可观测线索。
// 配置了可信反向代理时大概率由代理终结 TLS（合法部署），降级为 INFO。
func logSecureCookiesHint(logger *slog.Logger, cfg config.Config) {
	if !cfg.Auth.SecureCookies {
		return
	}
	if len(cfg.Server.TrustedProxies) == 0 {
		logger.Warn("secure_cookies_over_plain_http", "hint", "auth.secureCookies=true 且未配置 server.trustedProxies：浏览器经 HTTP 直连将拒收 Secure cookie（登录循环）；若由 HTTPS 反向代理终结 TLS，请配置 trustedProxies 以降级本提示")
		return
	}
	logger.Info("secure_cookies_enabled_behind_proxy", "hint", "假定 TLS 由可信反向代理终结；直连 HTTP 时浏览器将拒收会话 cookie")
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

// reloadSettings lets either document converge even when the other's apply fails.
func (a *Application) reloadSettings(ctx context.Context) error {
	err := a.settings.ReloadPersisted(ctx)
	if a.qualityTunables != nil {
		err = errors.Join(err, a.qualityTunables.ReloadPersisted(ctx))
	}
	return err
}
