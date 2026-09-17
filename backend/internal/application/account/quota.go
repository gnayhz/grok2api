package account

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type quotaRefreshResult struct {
	Credential accountdomain.Credential
	Windows    []accountdomain.QuotaWindow
	Modes      []string
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
				// 手动耗尽同样使用 domain owner 的有界探测期限，不引入第二套规则。
				if deadline, ok := accountdomain.QuotaWindowProbeAt(window, s.now()); ok {
					resetAt = &deadline
				}
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
	if s.quotaQueue == nil {
		return nil
	}
	now := s.now()
	// 恢复资格由 domain owner 规则唯一判定：不控制路由(Console 计费快照)、未耗尽
	// 或没有有界重试期限的窗口都不持有恢复事件，历史遗留事件在此取消。
	if !accountdomain.QuotaWindowDeservesRecovery(providerValue, window, now) {
		if err := s.quotaQueue.CancelQuotaRecovery(ctx, accountID, window.Mode); err != nil {
			return fmt.Errorf("取消额度恢复事件: %w", err)
		}
		return nil
	}
	// 资格判定已保证存在有界期限，这里只取同一 owner 规则给出的探测时间。
	deadline, _ := accountdomain.QuotaWindowProbeAt(window, now)
	if err := s.quotaQueue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: accountID, Mode: window.Mode, DueAt: deadline}); err != nil {
		return fmt.Errorf("安排额度恢复事件: %w", err)
	}
	return nil
}

func (s *Service) ListDueWebQuotaWindows(ctx context.Context, now time.Time, limit int) ([]accountdomain.QuotaWindow, error) {
	return s.accounts.ListDueQuotaWindows(ctx, now, repository.DueQuotaWindowQuery{Limit: limit, Provider: accountdomain.ProviderWeb})
}

func (s *Service) ListDueQuotaWindows(ctx context.Context, now time.Time, limit int, after *repository.QuotaWindowCursor) ([]accountdomain.QuotaWindow, error) {
	return s.accounts.ListDueQuotaWindows(ctx, now, repository.DueQuotaWindowQuery{Limit: limit, After: after})
}

func isWebChatQuotaMode(mode string) bool {
	return accountdomain.IsWebChatQuotaMode(mode)
}

func isConsoleUsageQuotaMode(mode string) bool {
	return accountdomain.IsConsoleUsageQuotaMode(mode)
}

func isWebImagineQuotaMode(mode string) bool {
	return accountdomain.IsWebImagineQuotaMode(mode)
}

// SyncAllBillingWithProgress 尽力刷新全部启用账号，单个账号失败不阻断其他账号。
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

func (s *Service) SyncAllWebQuotasWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, error) {
	return s.syncAllQuotasWithProgress(ctx, accountdomain.ProviderWeb, "web_quota_sync", progress)
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
		_, err := s.RefreshQuota(workCtx, id)
		return err
	})
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
