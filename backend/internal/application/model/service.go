package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

const defaultModelSyncWorkers = 25

// modelSyncRunTimeout bounds a detached full-account sync run. The run is
// decoupled from the triggering SSE connection; without a cap a leaked worker
// would hold the singleflight slot forever and block all future syncs.
const modelSyncRunTimeout = time.Hour
const syncFailurePersistTimeout = 5 * time.Second

var maxModelBatchSize = repository.MaxPageSize * len(modeldomain.Capabilities())

var (
	ErrInvalidFilter = errors.New("模型筛选条件无效")
	ErrInvalidInput  = errors.New("模型参数无效")
	ErrNotFound      = errors.New("模型不存在")
	ErrConflict      = errors.New("模型名称冲突")
	ErrClosed        = errors.New("模型同步服务已关闭")
)

type UpdateInput struct {
	PublicID   *string
	Enabled    *bool
	AccountIDs *[]uint64
}

type CreateInput struct {
	PublicID      string
	Provider      account.Provider
	UpstreamModel string
	Capability    modeldomain.Capability
	Enabled       bool
	AccountIDs    []uint64
}

type AccountOption struct {
	ID   uint64
	Name string
}

type RouteGroup struct {
	Routes               []modeldomain.Route
	EndpointCapabilities []string
}

type ListFilter struct {
	Provider    string
	Providers   []string
	Tiers       []string
	Status      string
	ActiveScope bool
	Sort        repository.SortQuery
}

type SyncProgressObserver func(completed, total int)

// AccountFacts 是目录读取的账号事实面：仅读取能力，无任何账号写方法。
// 目录不得通过本面导入、更新或删除账号。
type AccountFacts interface {
	Get(ctx context.Context, id uint64) (account.Credential, error)
	List(ctx context.Context, query repository.AccountListQuery) ([]account.Credential, int64, error)
	ListEnabled(ctx context.Context, provider account.Provider) ([]account.Credential, error)
	CountProviderAccountsByIDs(ctx context.Context, provider account.Provider, ids []uint64) (int64, error)
	GetBilling(ctx context.Context, accountID uint64) (account.Billing, error)
}

// Service 负责上游模型发现、内部来源路由与对外模型名称维护。
// CredentialEnsurer is the single account capability the catalog needs:
// materialize a usable credential before a capability sync probe. The full
// account service (imports, conversion, administration) stays outside.
type CredentialEnsurer interface {
	EnsureCredential(ctx context.Context, value account.Credential, force bool) (account.Credential, error)
}

type Service struct {
	models    repository.ModelRepository
	accounts  AccountFacts
	account   CredentialEnsurer
	providers provider.Registry
	bulkPool  *batch.Pool
	logger    *slog.Logger
	syncAll   singleflight.Group

	// syncRunMu guards the shared progress snapshot of the in-flight full sync.
	// The sync itself is detached from any single SSE connection (see
	// SyncObserved): browser disconnects no longer abort a run over a very
	// large account pool. Observers publish here so a reconnecting client can resume displaying
	// progress via SyncProgress.
	syncRunMu       sync.RWMutex
	syncRunActive   bool
	syncClosing     bool
	syncRunCancel   context.CancelFunc
	syncRunDone     chan struct{}
	accountSyncRuns map[uint64]accountSyncRun
	syncCompleted   int
	syncTotal       int
	syncErr         error
}

func NewService(models repository.ModelRepository, accounts AccountFacts, accountService CredentialEnsurer, providers provider.Registry) *Service {
	return &Service{models: models, accounts: accounts, account: accountService, providers: providers, bulkPool: batch.NewPool(defaultModelSyncWorkers), logger: slog.Default()}
}

func (s *Service) SetBulkPool(pool *batch.Pool) {
	if pool != nil {
		s.bulkPool = pool
	}
}

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Service) List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]modeldomain.Route, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	if !validProviderFilter(filter.Provider) || !validProviderFilters(filter.Providers) || !validTierFilters(filter.Tiers) || !validModelFilter(filter.Status, "", "enabled", "disabled") || !repository.IsValidSort(filter.Sort, "publicId", "upstreamModel", "status", "provider", "accountSupport", "lastSyncedAt") {
		return nil, 0, ErrInvalidFilter
	}
	var enabled *bool
	if filter.Status != "" {
		value := filter.Status == "enabled"
		enabled = &value
	}
	return s.models.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: search, Sort: filter.Sort}, Filter: repository.ModelListFilter{Provider: filter.Provider, Providers: filter.Providers, Tiers: filter.Tiers, Enabled: enabled, ActiveScope: filter.ActiveScope}})
}

func (s *Service) ListGroups(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]RouteGroup, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	if !validProviderFilter(filter.Provider) || !validModelFilter(filter.Status, "", "enabled", "disabled") || !repository.IsValidSort(filter.Sort, "publicId", "upstreamModel", "status", "provider", "accountSupport", "lastSyncedAt") {
		return nil, 0, ErrInvalidFilter
	}
	var enabled *bool
	if filter.Status != "" {
		value := filter.Status == "enabled"
		enabled = &value
	}
	values, total, err := s.models.ListGroups(ctx, repository.ModelListQuery{
		Page:   repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: search, Sort: filter.Sort},
		Filter: repository.ModelListFilter{Provider: filter.Provider, Enabled: enabled},
	})
	if err != nil {
		return nil, 0, err
	}
	groups := make([]RouteGroup, 0, len(values))
	for _, value := range values {
		groups = append(groups, RouteGroup{Routes: value.Routes, EndpointCapabilities: s.endpointCapabilities(value.Routes)})
	}
	return groups, total, nil
}

func (s *Service) endpointCapabilities(routes []modeldomain.Route) []string {
	if len(routes) == 0 || s.providers == nil {
		return nil
	}
	definition, ok := s.providers.Definition(routes[0].Provider)
	if !ok {
		return nil
	}
	return endpointCapabilitiesForDefinition(routes, definition)
}

func endpointCapabilitiesForDefinition(routes []modeldomain.Route, definition provider.Definition) []string {
	available := make(map[string]bool, 6)
	for _, route := range routes {
		if !modeldomain.SupportsCapability(route.Provider, route.UpstreamModel, route.Capability) {
			continue
		}
		switch route.Capability {
		case modeldomain.CapabilityResponses, modeldomain.CapabilityChat:
			available["completions"] = definition.Conversation.ChatCompletions
			available["responses"] = definition.Conversation.Responses
			available["messages"] = definition.Conversation.Messages
		case modeldomain.CapabilityImage:
			available["image"] = definition.Media.ImageGeneration
		case modeldomain.CapabilityImageEdit:
			available["image_edit"] = definition.Media.ImageEdit
		case modeldomain.CapabilityVideo:
			available["video"] = definition.Media.VideoGeneration
		case modeldomain.CapabilityTTS:
			available["tts"] = definition.Media.TTS
		case modeldomain.CapabilitySTT:
			available["stt"] = definition.Media.STT
		case modeldomain.CapabilityRealtime:
			available["realtime"] = definition.Media.Realtime
		}
	}
	order := []string{"completions", "responses", "messages", "image", "image_edit", "video", "tts", "stt", "realtime"}
	result := make([]string, 0, len(order))
	for _, capability := range order {
		if available[capability] {
			result = append(result, capability)
		}
	}
	return result
}

func validProviderFilter(value string) bool {
	return value == "" || account.Provider(value).IsValid()
}

func validProviderFilters(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !account.Provider(value).IsValid() {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validTierFilters(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "free" && value != "super" {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validModelFilter(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (s *Service) ListEnabled(ctx context.Context) ([]modeldomain.Route, error) {
	return s.models.ListEnabled(ctx)
}

func (s *Service) Get(ctx context.Context, id uint64) (modeldomain.Route, error) {
	return s.models.Get(ctx, id)
}

// GetByPublicID 每次读取共享主数据库，保证多实例下的路由禁用立即生效。
func (s *Service) GetByPublicID(ctx context.Context, publicID string) (modeldomain.Route, error) {
	return s.models.GetByPublicID(ctx, publicID)
}

// GetByPublicIDCandidates preserves the repository distinction between an absent
// name and a configured but unavailable name, including persisted aliases.
func (s *Service) GetByPublicIDCandidates(ctx context.Context, publicID string) ([]modeldomain.Route, error) {
	return s.models.GetByPublicIDCandidates(ctx, publicID)
}

// HasEnabledRouteByPublicID 透传仓储的存在性查询（不带账号可用性谓词），
// 供网关在候选为空时区分「模型不存在」与「无可用账号」。
func (s *Service) HasEnabledRouteByPublicID(ctx context.Context, publicID string) (bool, error) {
	return s.models.HasEnabledRouteByPublicID(ctx, publicID)
}

func (s *Service) GetByProviderUpstream(ctx context.Context, providerValue account.Provider, upstreamModel string) (modeldomain.Route, error) {
	return s.models.GetByProviderUpstream(ctx, providerValue, upstreamModel)
}

func (s *Service) Create(ctx context.Context, input CreateInput) (modeldomain.Route, error) {
	publicID, validPublicID := modeldomain.NormalizeExternalPublicID(input.Provider, input.PublicID)
	if !validPublicID {
		return modeldomain.Route{}, invalidInput("publicId 不能为空、不能携带其他 Provider 前缀，且长度不能超过 255 个字符")
	}
	upstreamModel, validUpstreamModel := modeldomain.NormalizeUpstreamModel(input.Provider, input.UpstreamModel)
	if !validUpstreamModel {
		return modeldomain.Route{}, invalidInput("upstreamModel 必须属于所选 Provider 且长度为 1-255 个字符")
	}
	definition, err := s.validateProviderCapability(input.Provider, input.Capability)
	if err != nil {
		return modeldomain.Route{}, err
	}
	if !modeldomain.SupportsCapability(input.Provider, upstreamModel, input.Capability) {
		return modeldomain.Route{}, invalidInput(fmt.Sprintf("%s 上游模型 %s 不支持 %s 能力", definition.ModelNamespace, upstreamModel, input.Capability))
	}
	accountIDs, err := s.validateBoundAccounts(ctx, input.Provider, input.AccountIDs)
	if err != nil {
		return modeldomain.Route{}, err
	}
	value := modeldomain.Route{
		PublicID: publicID, Provider: input.Provider, UpstreamModel: upstreamModel,
		Capability: input.Capability, Origin: modeldomain.OriginManual, Enabled: input.Enabled,
	}
	created, err := s.models.Create(ctx, value, accountIDs)
	return created, mapRepositoryError(err)
}

func (s *Service) Update(ctx context.Context, id uint64, input UpdateInput) (modeldomain.Route, error) {
	value, err := s.models.Get(ctx, id)
	if err != nil {
		return modeldomain.Route{}, mapRepositoryError(err)
	}
	if input.PublicID != nil {
		publicID, ok := modeldomain.NormalizeExternalPublicID(value.Provider, *input.PublicID)
		if !ok {
			return modeldomain.Route{}, invalidInput("publicId 不能为空、不能携带其他 Provider 前缀，且长度不能超过 255 个字符")
		}
		input.PublicID = &publicID
	}
	var accountIDs *[]uint64
	if input.AccountIDs != nil {
		validated, validateErr := s.validateBoundAccounts(ctx, value.Provider, *input.AccountIDs)
		if validateErr != nil {
			return modeldomain.Route{}, validateErr
		}
		accountIDs = &validated
	}
	updated, err := s.models.Patch(ctx, id, modeldomain.RoutePatch{PublicID: input.PublicID, Enabled: input.Enabled, AccountIDs: accountIDs})
	return updated, mapRepositoryError(err)
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	if id == 0 {
		return invalidInput("模型 ID 无效")
	}
	return mapRepositoryError(s.models.Delete(ctx, id))
}

func (s *Service) BatchDelete(ctx context.Context, ids []uint64) (int64, error) {
	values, err := normalizeModelRouteBatchIDs(ids)
	if err != nil {
		return 0, err
	}
	return s.models.DeleteMany(ctx, values)
}

func (s *Service) ListBindableAccounts(ctx context.Context, providerValue account.Provider, page, pageSize int, search string) ([]AccountOption, int64, error) {
	if !providerValue.IsValid() {
		return nil, 0, invalidInput("账号来源无效")
	}
	page, pageSize = normalizePage(page, pageSize)
	values, total, err := s.accounts.List(ctx, repository.AccountListQuery{
		Page:   repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: search},
		Filter: repository.AccountListFilter{Provider: string(providerValue)},
	})
	if err != nil {
		return nil, 0, err
	}
	result := make([]AccountOption, 0, len(values))
	for _, value := range values {
		result = append(result, AccountOption{ID: value.ID, Name: value.Name})
	}
	return result, total, nil
}

func (s *Service) validateProviderCapability(providerValue account.Provider, capability modeldomain.Capability) (provider.Definition, error) {
	if !providerValue.IsValid() || s.providers == nil {
		return provider.Definition{}, invalidInput("provider 无效")
	}
	definition, ok := s.providers.Definition(providerValue)
	if !ok {
		return provider.Definition{}, invalidInput("provider 未注册能力定义")
	}
	if !definition.SupportsModelCapability(capability) {
		return provider.Definition{}, invalidInput(fmt.Sprintf("%s 不支持 %s 能力", definition.ModelNamespace, capability))
	}
	return definition, nil
}

func (s *Service) validateBoundAccounts(ctx context.Context, providerValue account.Provider, ids []uint64) ([]uint64, error) {
	if len(ids) > 1000 {
		return nil, invalidInput("单个模型最多绑定 1000 个账号")
	}
	unique := make(map[uint64]struct{}, len(ids))
	result := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, invalidInput("绑定账号 ID 无效")
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return result, nil
	}
	count, err := s.accounts.CountProviderAccountsByIDs(ctx, providerValue, result)
	if err != nil {
		return nil, err
	}
	if count != int64(len(result)) {
		return nil, invalidInput("绑定账号不存在或与模型来源不匹配")
	}
	return result, nil
}

// BatchSetEnabled 批量更新模型路由启停状态。
func (s *Service) BatchSetEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error) {
	values, err := normalizeModelRouteBatchIDs(ids)
	if err != nil {
		return 0, err
	}
	updated, err := s.models.UpdateManyEnabled(ctx, values, enabled)
	return updated, err
}

// SyncObserved 执行全量模型同步，并按已完成账号数报告进度。
// SyncRunSnapshot describes the in-flight (or last finished) full sync for
// reconnecting progress displays. Active=false with Err=nil means a previous
// run completed successfully (Completed==Total).
type SyncRunSnapshot struct {
	Active    bool
	Completed int
	Total     int
	Err       error
}

// SyncProgress returns the shared progress snapshot of the full-account sync.
func (s *Service) SyncProgress() SyncRunSnapshot {
	s.syncRunMu.RLock()
	defer s.syncRunMu.RUnlock()
	return SyncRunSnapshot{Active: s.syncRunActive, Completed: s.syncCompleted, Total: s.syncTotal, Err: s.syncErr}
}

func (s *Service) storeSyncProgress(completed, total int) {
	s.syncRunMu.Lock()
	s.syncCompleted, s.syncTotal = completed, total
	s.syncRunMu.Unlock()
}

// SyncObserved starts (or joins) the full-account model sync and streams
// progress to observer until completion. The sync runs on a context detached
// from the caller: with very large account pools a run takes tens of minutes, and a
// browser/CDN disconnect on the SSE progress connection must not abort the
// underlying work — the run keeps going and any client can re-attach via
// SyncProgress (the singleflight group joins concurrent callers to the same
// in-flight run). A timeout bounds the detached run against leaks.
func (s *Service) SyncObserved(ctx context.Context, observer SyncProgressObserver) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.syncRunMu.RLock()
	closing := s.syncClosing
	s.syncRunMu.RUnlock()
	if closing {
		return 0, ErrClosed
	}
	result := s.syncAll.DoChan("all", func() (any, error) {
		s.syncRunMu.Lock()
		// The singleflight function runs asynchronously. Close can win after
		// the watcher entered DoChan but before this goroutine starts.
		if s.syncClosing {
			s.syncRunMu.Unlock()
			return 0, ErrClosed
		}
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelSyncRunTimeout)
		done := make(chan struct{})
		s.syncRunCancel, s.syncRunDone = cancel, done
		s.syncRunActive = true
		s.syncCompleted, s.syncTotal, s.syncErr = 0, 0, nil
		s.syncRunMu.Unlock()
		var synced int
		err := batch.Do(runCtx, func(workCtx context.Context) error {
			var err error
			synced, err = s.syncAllAccounts(workCtx, func(completed, total int) {
				s.storeSyncProgress(completed, total)
				if observer != nil {
					observer(completed, total)
				}
			})
			return err
		})
		cancel()
		s.syncRunMu.Lock()
		s.syncRunActive = false
		s.syncErr = err
		s.syncRunCancel = nil
		close(done)
		s.syncRunMu.Unlock()
		return synced, err
	})
	select {
	case <-ctx.Done():
		// Caller gave up watching; the detached run continues and the snapshot
		// stays queryable. Join it again by calling SyncObserved/SyncProgress.
		return 0, ctx.Err()
	case value := <-result:
		if value.Err != nil {
			return 0, value.Err
		}
		return value.Val.(int), nil
	}
}

// Close stops accepting detached syncs, cancels all registered runs and waits
// for their workers and final state writes. Watcher cancellation alone keeps a full
// run alive. A close timeout leaves dependencies owned by the run; callers must
// retain them and retry Close after it finishes.
func (s *Service) Close(ctx context.Context) error {
	s.syncRunMu.Lock()
	s.syncClosing = true
	var runs []accountSyncRun
	if s.syncRunDone != nil {
		runs = append(runs, accountSyncRun{cancel: s.syncRunCancel, done: s.syncRunDone})
	}
	for _, run := range s.accountSyncRuns {
		runs = append(runs, run)
	}
	s.syncRunMu.Unlock()
	for _, run := range runs {
		if run.cancel != nil {
			run.cancel()
		}
	}
	for _, run := range runs {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *Service) syncAllAccounts(ctx context.Context, observer SyncProgressObserver) (int, error) {
	if s.providers == nil {
		return 0, fmt.Errorf("Provider 注册表未初始化")
	}
	providerValues := s.providers.Providers()
	if len(providerValues) == 0 {
		return 0, fmt.Errorf("没有已注册的 Provider")
	}
	credentials := make([]account.Credential, 0)
	for _, providerValue := range providerValues {
		values, err := s.accounts.ListEnabled(ctx, providerValue)
		if err != nil {
			return 0, err
		}
		credentials = append(credentials, values...)
	}
	if len(credentials) == 0 {
		return 0, fmt.Errorf("没有可用于模型同步的账号")
	}
	if observer != nil {
		observer(0, len(credentials))
	}
	var completed atomic.Int64
	results, summary, runErr := batch.MapObserved(ctx, credentials, batch.Options{Workers: s.bulkPool.Limit(), Pool: s.bulkPool}, func(workCtx context.Context, value account.Credential) ([]string, error) {
		adapter, ok := s.providers.Models(value.Provider)
		if !ok {
			return nil, fmt.Errorf("Provider %s 未注册模型同步能力", value.Provider)
		}
		return s.syncAccountCapabilities(workCtx, value, adapter)
	}, func(_ int, _ batch.Result[[]string]) {
		if observer != nil {
			observer(int(completed.Add(1)), len(credentials))
		}
	})
	pool := s.bulkPool.Snapshot()
	s.logger.Info("model_bulk_sync_completed", "total", summary.Total, "submitted", summary.Submitted, "succeeded", summary.Succeeded, "failed", summary.Failed, "panicked", summary.Panicked, "duration_ms", summary.Duration.Milliseconds(), "canceled", summary.Canceled, "pool_limit", pool.Limit, "pool_active", pool.Active, "pool_queued", pool.Queued, "pool_peak", pool.Peak, "error", runErr)
	if runErr != nil {
		return 0, runErr
	}

	uniqueModels := make(map[account.Provider]map[string]struct{}, len(providerValues))
	succeeded := 0
	var lastErr error
	for index, result := range results {
		if result.Err != nil {
			var panicErr *batch.PanicError
			if errors.As(result.Err, &panicErr) {
				s.logger.Error("model_sync_panicked", "account_id", credentials[index].ID, "error", panicErr, "stack", string(panicErr.Stack))
			}
			lastErr = result.Err
			continue
		}
		succeeded++
		providerModels := uniqueModels[credentials[index].Provider]
		if providerModels == nil {
			providerModels = make(map[string]struct{})
			uniqueModels[credentials[index].Provider] = providerModels
		}
		for _, value := range result.Value {
			value = strings.TrimSpace(value)
			if value != "" {
				providerModels[value] = struct{}{}
			}
		}
	}
	if succeeded == 0 {
		if lastErr != nil {
			return 0, lastErr
		}
		return 0, fmt.Errorf("没有账号成功同步模型")
	}
	syncedModels := 0
	for _, providerValue := range providerValues {
		providerModels := uniqueModels[providerValue]
		if len(providerModels) == 0 {
			continue
		}
		models := make([]string, 0, len(providerModels))
		for value := range providerModels {
			models = append(models, value)
		}
		if err := s.publishDiscovered(ctx, providerValue, models); err != nil {
			return 0, err
		}
		syncedModels += len(models)
	}
	return syncedModels, nil
}

// HasSuccessfulAccountSync 判断账号是否已有成功模型能力快照，不触发上游请求。
func (s *Service) HasSuccessfulAccountSync(ctx context.Context, accountID uint64) (bool, error) {
	return s.models.HasSuccessfulAccountSync(ctx, accountID)
}

// SyncAccount 只同步指定账号，并把该账号发现的模型合并到公开路由目录。
func (s *Service) SyncAccount(ctx context.Context, accountID uint64) (int, error) {
	credential, err := s.accounts.Get(ctx, accountID)
	if err != nil {
		return 0, err
	}
	adapter, ok := s.providers.Models(credential.Provider)
	if !ok {
		return 0, fmt.Errorf("Provider %s 未注册", credential.Provider)
	}
	models, err := s.syncAccountCapabilities(ctx, credential, adapter)
	if err != nil {
		return 0, err
	}
	if err := s.publishDiscovered(ctx, credential.Provider, models); err != nil {
		return 0, err
	}
	return len(models), nil
}

// SyncAccounts 使用共享同步池追赶指定账号的模型能力，不扩大为全量同步。
func (s *Service) SyncAccounts(ctx context.Context, accountIDs []uint64) (int, int, error) {
	ids, err := normalizeBatchIDs(accountIDs)
	if err != nil {
		return 0, 0, err
	}
	results, summary, runErr := batch.Map(ctx, ids, batch.Options{Workers: s.bulkPool.Limit(), Pool: s.bulkPool}, func(workCtx context.Context, id uint64) (int, error) {
		return s.SyncAccount(workCtx, id)
	})
	for index, result := range results {
		if result.Err == nil {
			continue
		}
		var panicErr *batch.PanicError
		if errors.As(result.Err, &panicErr) {
			s.logger.Error("model_startup_sync_panicked", "account_id", ids[index], "error", panicErr, "stack", string(panicErr.Stack))
		}
	}
	s.logger.Info("model_startup_sync_completed", "total", summary.Total, "succeeded", summary.Succeeded, "failed", summary.Failed, "canceled", summary.Canceled, "error", runErr)
	return summary.Succeeded, summary.Failed, runErr
}

func (s *Service) syncAccountCapabilities(ctx context.Context, value account.Credential, adapter provider.ModelCatalogAdapter) ([]string, error) {
	ref, err := s.models.BeginAccountCapabilitySync(ctx, value.ID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	credential, err := s.account.EnsureCredential(ctx, value, false)
	if err != nil {
		s.markCapabilitySyncFailed(ref, value.CredentialRef(), err)
		return nil, err
	}
	values, err := adapter.ListModels(ctx, credential)
	if err != nil {
		s.markCapabilitySyncFailed(ref, credential.CredentialRef(), err)
		return nil, err
	}
	models := normalizeDiscoveredModels(values)
	if normalizer, ok := adapter.(provider.AccountModelCapabilityNormalizer); ok {
		var billing *account.Billing
		snapshot, billingErr := s.accounts.GetBilling(ctx, credential.ID)
		if billingErr == nil {
			billing = &snapshot
		} else if !errors.Is(billingErr, repository.ErrNotFound) {
			// Billing 不存在按 Unknown 处理；其他仓储错误保留失败语义。
			s.markCapabilitySyncFailed(ref, credential.CredentialRef(), billingErr)
			return nil, billingErr
		}
		models = normalizeDiscoveredModels(normalizer.NormalizeAccountModelCapabilities(models, billing, credential))
	}
	if err := s.models.CompleteAccountCapabilitySync(ctx, ref, modeldomain.CapabilitySyncResult{Credential: credential.CredentialRef(), Models: models}); err != nil {
		s.markCapabilitySyncFailed(ref, credential.CredentialRef(), err)
		return nil, err
	}
	return models, nil
}

func normalizeDiscoveredModels(values []string) []string {
	unique := make(map[string]struct{}, len(values))
	models := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := unique[value]; exists {
			continue
		}
		unique[value] = struct{}{}
		models = append(models, value)
	}
	return models
}

// markCapabilitySyncFailed 使用独立短超时保存失败状态，避免请求取消后丢失账号能力诊断信息。
func (s *Service) markCapabilitySyncFailed(ref modeldomain.CapabilitySyncRef, credential account.CredentialRef, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), syncFailurePersistTimeout)
	defer cancel()
	_ = s.models.CompleteAccountCapabilitySync(ctx, ref, modeldomain.CapabilitySyncResult{Credential: credential, Err: cause})
}

func normalizePage(page, pageSize int) (int, int) {
	return repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
}

func normalizeBatchIDs(ids []uint64) ([]uint64, error) {
	return normalizeIDs(ids, repository.MaxPageSize, "模型")
}

func normalizeModelRouteBatchIDs(ids []uint64) ([]uint64, error) {
	return normalizeIDs(ids, maxModelBatchSize, "模型能力路由")
}

func normalizeIDs(ids []uint64, limit int, label string) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, invalidInput("至少选择一个模型")
	}
	if len(ids) > limit {
		return nil, invalidInput(fmt.Sprintf("单次最多处理 %d 条%s", limit, label))
	}
	seen := make(map[uint64]struct{}, len(ids))
	result := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, invalidInput("模型 ID 无效")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}

// invalidInput 为可安全返回给管理端的模型参数错误附加稳定语义。
func invalidInput(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, message)
}

// mapRepositoryError 将仓储错误转换为模型应用错误。
func mapRepositoryError(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, repository.ErrConflict) {
		return ErrConflict
	}
	return err
}
