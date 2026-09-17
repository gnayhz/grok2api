package account

import (
	"context"
	"slices"
	"strings"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/cfcookies"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

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

type ListFilter struct {
	Quality   string
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

func (s *Service) List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]View, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	if (filter.Provider != "" && !accountdomain.Provider(filter.Provider).IsValid()) ||
		!slices.Contains([]string{"", "free", "paid", "unknown", "auto", "basic", "super", "heavy"}, filter.QuotaType) ||
		!slices.Contains([]string{"", "active", "disabled", "reauthRequired", "cooldown", "waitingReset", "probing", "risk"}, filter.Status) ||
		!slices.Contains([]string{"", "refreshable", "unrefreshable"}, filter.Renewal) ||
		!slices.Contains([]string{"", "flagged", "normal"}, filter.Risk) ||
		!slices.Contains([]string{"", "restricted", "remanded", "sentenced", "clear"}, filter.Quality) ||
		!slices.Contains([]string{"", "nsfwEnabled", "nsfwDisabled", "termsAccepted", "termsNotAccepted", "allAccepted", "allNotAccepted"}, filter.Agreement) ||
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
	states, err := s.readQualityStates(ctx)
	if err != nil {
		return nil, 0, err
	}
	applyQualityFilter(&repositoryFilter, filter.Quality, states)
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
	observedTokens, err := s.audits.SumTokensByAccountsSince(ctx, accountIDs, s.now().Add(-freeUsageWindow))
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
		view := View{Quality: qualityState(states, value.ID), Credential: value, BuildBotFlagged: metadata.BuildBotFlagged, BuildBotFlagSource: metadata.BuildBotFlagSource}
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
