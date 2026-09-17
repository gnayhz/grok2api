package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// accountLayerDeps 汇集账号域装配所需的外部协作者(分域 wiring)。
type accountLayerDeps struct {
	cfg                config.Config
	logger             *slog.Logger
	accountRepo        repository.AccountRepository
	modelRepo          repository.ModelRepository
	auditRepo          repository.AuditRepository
	deviceSessions     repository.DeviceSessionRepository
	sticky             repository.StickySessionRepository
	providers          provider.Registry
	cipher             security.Cryptor
	refreshLock        repository.DistributedLock
	concurrency        repository.ConcurrencyLimiter
	bulkPool           *batch.Pool
	quotaQueue         repository.QuotaRecoveryQueue
	quotaRefreshState  repository.QuotaRefreshCoordinator
	observedModelStore repository.ObservedModelStateRepository
	cliAdapter         *cliprovider.Adapter
}

// accountLayer 是账号域装配返回的具名依赖与 owned pools。
type accountLayer struct {
	importPool      *batch.Pool
	conversionPool  *batch.Pool
	syncPool        *batch.Pool
	refreshPool     *batch.Pool
	detectPool      *batch.Pool
	accountService  *accountapp.Service
	modelService    *modelapp.Service
	accountSync     *accountsyncapp.Service
	quotaRecoveries int
}

// bootstrapAccountLayer 装配账号域(分域 wiring):批量池、账号服务、
// Web 额度恢复重放、模型目录与账号同步。App.New 调用一次并持有返回的
// 具名依赖;本文件不承载业务规则。
func bootstrapAccountLayer(ctx context.Context, d accountLayerDeps) (*accountLayer, error) {
	importPool := batch.NewSharedChildPool(d.cfg.Batch.ImportConcurrency, d.concurrency, "bulk:import", d.bulkPool)
	conversionPool := batch.NewSharedChildPool(d.cfg.Batch.ConversionConcurrency, d.concurrency, "bulk:conversion", d.bulkPool)
	syncPool := batch.NewSharedChildPool(d.cfg.Batch.SyncConcurrency, d.concurrency, "bulk:sync", d.bulkPool)
	refreshPool := batch.NewSharedChildPool(d.cfg.Batch.RefreshConcurrency, d.concurrency, "bulk:refresh", d.bulkPool)
	// detectPool 固定 32 并发，与额度同步/续期隔离，避免全量探测挤占维护任务。
	detectPool := batch.NewSharedChildPool(32, d.concurrency, "bulk:detect", d.bulkPool)
	for _, pool := range []*batch.Pool{importPool, conversionPool, syncPool, refreshPool, detectPool} {
		pool.UpdateJitter(d.cfg.Batch.RandomDelay.Value())
	}
	accountService := accountapp.NewService(d.accountRepo, d.auditRepo, d.deviceSessions, d.sticky, d.providers, d.cipher, security.RandomTokenSource{}, providerimpl.NewRejectionClassifier(), providerimpl.NewResponsesProbeInspector(), d.refreshLock)
	d.cliAdapter.SetFallbackMarker(accountService)
	accountService.SetLogger(d.logger)
	accountService.UpdateAutoCleanConfig(accountAutoCleanConfig(d.cfg.Accounts))
	accountService.SetConcurrencyLimiter(d.concurrency)
	accountService.SetQuotaRecoveryQueue(d.quotaQueue)
	accountService.SetQuotaRefreshCoordinator(d.quotaRefreshState)
	accountService.SetObservedModelStore(d.observedModelStore)
	accountService.SetTaskPools(conversionPool, syncPool, refreshPool)
	accountService.SetDetectPool(detectPool)
	if err := accountService.RebuildBuildBotFlagIndex(ctx); err != nil {
		return nil, fmt.Errorf("重建 Build 风控路由索引: %w", err)
	}
	windows, err := d.accountRepo.ListQuotaRecoveryWindows(ctx, 100000)
	if err != nil {
		return nil, fmt.Errorf("加载额度恢复候选窗口: %w", err)
	}
	restored, err := replayQuotaRecoveryWindows(ctx, d.quotaQueue, windows, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	modelService := modelapp.NewService(d.modelRepo, d.accountRepo, accountService, d.providers)
	modelService.SetBulkPool(syncPool)
	modelService.SetLogger(d.logger)
	if err := modelService.PublishCatalogs(ctx); err != nil {
		return nil, err
	}
	accountSyncService := accountsyncapp.NewService(d.logger, accountService, accountService, accountService, accountService, accountService, accountService, accountService, modelService)
	accountSyncService.SetBulkPool(importPool)
	accountSyncService.UpdateConcurrency(d.cfg.Batch.ImportConcurrency)
	return &accountLayer{importPool: importPool, conversionPool: conversionPool, syncPool: syncPool, refreshPool: refreshPool, detectPool: detectPool, accountService: accountService, modelService: modelService, accountSync: accountSyncService, quotaRecoveries: restored}, nil
}

// replayQuotaRecoveryWindows 在启动时把持久化的恢复候选行写回恢复队列，返回真正
// 重建的事件数。资格与探测期限完全由 domain/account 的 owner 规则决定(与对账入口
// 同一规则)，这里只承载启动时序与错误包装；参数是存储返回的原始候选行，不做二次筛选。
func replayQuotaRecoveryWindows(ctx context.Context, queue repository.QuotaRecoveryQueue, windows []accountdomain.QuotaWindow, now time.Time) (int, error) {
	restored := 0
	for _, window := range windows {
		if !accountdomain.QuotaWindowDeservesRecovery(window.Provider, window, now) {
			continue
		}
		// 资格判定已保证存在有界期限。
		deadline, _ := accountdomain.QuotaWindowProbeAt(window, now)
		if err := queue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: window.AccountID, Mode: window.Mode, DueAt: deadline}); err != nil {
			return restored, fmt.Errorf("恢复额度事件: %w", err)
		}
		restored++
	}
	return restored, nil
}
