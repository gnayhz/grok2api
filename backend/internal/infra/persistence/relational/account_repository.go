package relational

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AccountRepository struct {
	db       *Database
	observer repository.InvalidationObserver
}

func NewAccountRepository(db *Database) *AccountRepository { return &AccountRepository{db: db} }

func (r *AccountRepository) SetInvalidationObserver(observer repository.InvalidationObserver) {
	r.observer = observer
}

func (r *AccountRepository) notifyInvalidation(ctx context.Context, event repository.InvalidationEvent) {
	if r.observer != nil {
		r.observer(ctx, event)
	}
}

type quotaBreakdownJSON struct {
	ProductCode  int     `json:"productCode"`
	UsagePercent float64 `json:"usagePercent"`
}

const (
	accountUpdateBatchSize      = 500
	accountNormalizedPlanCode   = `LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_code), ' ', ''), '_', ''), '-', ''), '+', 'plus'))`
	accountNormalizedPlanName   = `LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_name), ' ', ''), '_', ''), '-', ''), '+', 'plus'))`
	accountPaidPlanNames        = `'super', 'supergrok', 'supergrokpro', 'supergrokheavy', 'supergroklite', 'supergrokplus', 'grokpro', 'xpremium', 'xpremiumplus', 'apikey'`
	accountPaidPlanSignal       = `(` + accountNormalizedPlanCode + ` IN (` + accountPaidPlanNames + `) OR ` + accountNormalizedPlanName + ` IN (` + accountPaidPlanNames + `) OR substr(` + accountNormalizedPlanCode + `, 1, 9) = 'supergrok' OR substr(` + accountNormalizedPlanName + `, 1, 9) = 'supergrok')`
	accountFreePlanSignal       = `(LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_code), ' ', ''), '_', ''), '-', ''), '+', 'plus')) IN ('free', 'grokfree', 'freetier', 'basic', 'grokbasic', 'xbasic') OR LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_name), ' ', ''), '_', ''), '-', ''), '+', 'plus')) IN ('free', 'grokfree', 'freetier', 'basic', 'grokbasic', 'xbasic'))`
	accountPaidBillingSignals   = `(` + accountPaidPlanSignal + ` OR billing.monthly_limit > 0 OR billing.on_demand_cap > 0 OR billing.on_demand_used > 0 OR billing.prepaid_balance > 0)`
	accountPaidBillingPredicate = `EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = provider_accounts.id AND ` + accountPaidBillingSignals + `)`
	// 仅 grok_build 的管理员确认 Super entitlement；与 domain.IsBuildSuper 对齐。
	accountBuildSuperEntitledPredicate = `(provider_accounts.provider = 'grok_build' AND provider_accounts.build_super_entitled = TRUE)`
	accountBuildSuperPredicate         = `(` + accountPaidBillingPredicate + ` OR ` + accountBuildSuperEntitledPredicate + `)`
	accountInferredFreeBillingSignal   = `(TRIM(billing.plan_code) = '' AND TRIM(billing.plan_name) = '' AND billing.synced_at IS NOT NULL AND billing.monthly_limit = 0 AND billing.used = 0 AND billing.on_demand_cap = 0 AND billing.on_demand_used = 0 AND billing.prepaid_balance = 0 AND billing.credit_usage_percent = 0)`
	accountFreeBillingSignal           = `(` + accountFreePlanSignal + ` OR ` + accountInferredFreeBillingSignal + `)`
	accountFreeSignalPredicate         = `(provider_accounts.provider = 'grok_build' AND (LOWER(TRIM(provider_accounts.observed_model)) LIKE '%-build-free' OR EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = provider_accounts.id AND ` + accountFreeBillingSignal + `)))`
	accountRecoveryPredicate           = `EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status IN ('exhausted', 'probing'))`
	// providerQuotaExhaustedPredicate 与 domain/account.QuotaWindowControlsRouting
	// 的 weekly/console 分类对齐:SQL 侧为管理列表/状态投影的权威写,语义
	// 演变必须两处同步(与上方 IsBuildSuper 对齐注释同款约束)。
	providerQuotaExhaustedPredicate = `((provider_accounts.provider = 'grok_web' AND ((EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly') AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly' AND quota.remaining > 0)) OR (NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly') AND EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id) AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.remaining > 0)))) OR (provider_accounts.provider = 'grok_console' AND EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'console') AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'console' AND quota.remaining > 0)))`
	accountTypeSortExpression       = `CASE WHEN provider_accounts.provider = 'grok_web' THEN COALESCE((SELECT profile.tier FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id), 'auto') WHEN ` + accountBuildSuperPredicate + ` THEN 'paid' WHEN ` + accountFreeSignalPredicate + ` THEN 'free' ELSE 'unknown' END`
	accountStatusSortExpression     = `CASE WHEN provider_accounts.risk_status <> '' THEN 6 WHEN provider_accounts.enabled = FALSE THEN 4 WHEN provider_accounts.auth_status = 'reauthRequired' THEN 5 WHEN EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing') THEN 3 WHEN EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR ` + providerQuotaExhaustedPredicate + ` THEN 2 WHEN provider_accounts.cooldown_until > CURRENT_TIMESTAMP THEN 1 ELSE 0 END`
	// accountUnclassifiedRefreshReauthPredicate 表达"未分类刷新失败且未
	// 标记永久失败的账号不得被自动清理"的资格规则;业务口径归
	// domain/account,此处是它的 SQL 投影(清理年龄归 application)。
	accountUnclassifiedRefreshReauthPredicate = `NOT EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.refresh_permanent = FALSE AND credential.refresh_unclassified_auth_failures > 0)`
	missingConsoleAccountPredicate            = `NOT EXISTS (SELECT 1 FROM provider_accounts AS console_account WHERE console_account.provider = ? AND console_account.source_key = ('console-' || provider_accounts.source_key))`
)

func (r *AccountRepository) ListProviderAccountBatch(ctx context.Context, providerValue account.Provider, afterID uint64, limit int) ([]account.Credential, int64, error) {
	if limit < 1 {
		return []account.Credential{}, 0, nil
	}
	var total int64
	if afterID == 0 {
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("provider = ?", providerValue).Count(&total).Error; err != nil {
			return nil, 0, err
		}
	}
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).
		Preload("Credential").Preload("WebProfile").
		Where("provider = ? AND id > ?", providerValue, afterID).
		Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAccountDomain(row))
	}
	if err := r.attachAccountLinks(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// CountProviderAccountsByIDs 只校验账号主表归属，不加载额度、关联或审计数据。

// CountAvailableAmong counts IDs that currently match Summarize's available predicate.
func (r *AccountRepository) CountAvailableAmong(ctx context.Context, providerValue account.Provider, ids []uint64, now time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	const batchSize = 500
	var total int64
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		var count int64
		query := r.db.db.WithContext(ctx).Model(&accountModel{}).
			Where("provider = ? AND id IN ?", providerValue, ids[start:end])
		query = applyAccountStatusFilter(query, "active", now)
		if err := query.Count(&count).Error; err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

// CountBuildBotFlagged counts persisted Build risk metadata without loading an
// account-ID slice or credential material.
func (r *AccountRepository) CountBuildBotFlagged(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Joins("JOIN account_credentials AS credential ON credential.account_id = provider_accounts.id").
		Where("provider_accounts.provider = ? AND credential.build_bot_flag_source IN (1,2)", account.ProviderBuild).
		Count(&count).Error
	return count, err
}

// CountAvailableBuildBotFlagged uses the same availability predicate as
// Summarize without expanding a potentially unbounded ID list.
func (r *AccountRepository) CountAvailableBuildBotFlagged(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	query := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Joins("JOIN account_credentials AS credential ON credential.account_id = provider_accounts.id").
		Where("provider_accounts.provider = ? AND credential.build_bot_flag_source IN (1,2)", account.ProviderBuild)
	query = applyAccountStatusFilter(query, "active", now)
	err := query.Count(&count).Error
	return count, err
}

// ListBuildBotFlaggedAccountIDs reads persisted non-sensitive metadata only; it
// never loads or decrypts access tokens on the scheduling path.
func (r *AccountRepository) ListBuildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error) {
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
		Where("account.provider = ? AND credential.build_bot_flag_source IN (1,2)", account.ProviderBuild).
		Order("account.id ASC").
		Scan(&ids).Error
	return ids, err
}

// ListBuildBotFlagCredentialBatch returns the minimum projection required for
// startup backfill of the persisted risk source.
func (r *AccountRepository) ListBuildBotFlagCredentialBatch(ctx context.Context, afterID uint64, limit int) ([]repository.BuildBotFlagCredential, error) {
	if limit < 1 {
		return []repository.BuildBotFlagCredential{}, nil
	}
	var rows []struct {
		AccountID            uint64
		EncryptedAccessToken string
		StoredSource         int
	}
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id AS account_id, credential.encrypted_primary AS encrypted_access_token, credential.build_bot_flag_source AS stored_source").
		Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
		Where("account.provider = ? AND account.id > ?", account.ProviderBuild, afterID).
		Order("account.id ASC").Limit(limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make([]repository.BuildBotFlagCredential, 0, len(rows))
	for _, row := range rows {
		result = append(result, repository.BuildBotFlagCredential{
			AccountID: row.AccountID, EncryptedAccessToken: row.EncryptedAccessToken, StoredSource: row.StoredSource,
		})
	}
	return result, nil
}

// UpdateBuildBotFlagSources persists a bounded backfill batch transactionally.
func (r *AccountRepository) UpdateBuildBotFlagSources(ctx context.Context, values []repository.BuildBotFlagSourceUpdate) error {
	if len(values) == 0 {
		return nil
	}
	changed := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, value := range values {
			source := normalizeBuildBotFlagSource(account.ProviderBuild, value.Source)
			result := tx.Model(&accountCredentialModel{}).
				Where("account_id = ? AND encrypted_primary = ?", value.AccountID, value.ExpectedEncryptedAccessToken).
				Update("build_bot_flag_source", source)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected > 0 {
				changed = true
			}
		}
		return nil
	})
	if err == nil && changed {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, Provider: account.ProviderBuild})
	}
	return err
}

func (r *AccountRepository) Summarize(ctx context.Context, now time.Time) ([]repository.AccountSummary, error) {
	var rows []repository.AccountSummary
	selectFields := `
		provider,
		COUNT(*) AS total,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND NOT ` + accountRecoveryPredicate + ` AND NOT ` + providerQuotaExhaustedPredicate + ` AND risk_status = '' AND (cooldown_until IS NULL OR cooldown_until <= ?) THEN 1 ELSE 0 END) AS available,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND NOT ` + accountRecoveryPredicate + ` AND NOT ` + providerQuotaExhaustedPredicate + ` AND cooldown_until > ? THEN 1 ELSE 0 END) AS cooldown,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR ` + providerQuotaExhaustedPredicate + `) THEN 1 ELSE 0 END) AS waiting_reset,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing') THEN 1 ELSE 0 END) AS probing,
		SUM(CASE WHEN enabled = ? THEN 1 ELSE 0 END) AS disabled,
		SUM(CASE WHEN enabled = ? AND auth_status = ? THEN 1 ELSE 0 END) AS reauth_required,
		SUM(CASE WHEN risk_status <> '' AND NOT EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.build_bot_flag_source IN (1,2)) THEN 1 ELSE 0 END) AS risk_flagged`
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).Select(
		selectFields,
		true, account.AuthStatusActive, now,
		true, account.AuthStatusActive, now,
		true, account.AuthStatusActive,
		true, account.AuthStatusActive,
		false,
		true, account.AuthStatusReauthRequired,
	).Group("provider").Scan(&rows).Error
	return rows, err
}

// ListRoutingCandidates 批量加载账号、额度、恢复状态和目标模型能力，避免推理热路径按账号逐条查询。

// listRoutingCredentials loads only the account state required to decide which
// account to use. Provider secrets deliberately stay in account_credentials
// until a selected account is hydrated for the upstream call.

// listActiveProviderAccountRows avoids GORM association preloads for complete
// provider pools. Preload expands every parent key into an IN list and exceeds
// SQLite's variable limit for large pools. The fixed-shape JOIN queries below
// remain valid for both SQLite and PostgreSQL regardless of pool size.

// routingCredentialMetadataColumns contains all credential fields used for
// routing and execution decisions, but deliberately excludes the encrypted
// access token, refresh token, and Cloudflare cookie.
var routingCredentialMetadataColumns = []string{
	"account_id", "auth_type", "client_id", "expires_at", "refresh_due_at", "last_refresh_at",
	"refresh_failures", "last_refresh_error", "refresh_permanent", "build_bot_flag_source", "updated_at",
}

var routingBillingColumns = []string{
	"account_id", "plan_code", "plan_name", "monthly_limit", "used", "on_demand_cap", "on_demand_used", "prepaid_balance",
	"credit_usage_percent", "is_unified_billing_user", "on_demand_enabled", "top_up_method", "usage_period_type",
	"usage_period_start", "usage_period_end", "billing_period_start", "billing_period_end", "synced_at",
}

func (r *AccountRepository) getRoutingQuotaRecoveries(ctx context.Context, provider account.Provider) (map[uint64]account.QuotaRecovery, error) {
	result := make(map[uint64]account.QuotaRecovery)
	var rows []quotaRecoveryModel
	if err := r.db.db.WithContext(ctx).
		Table("account_quota_recovery AS recovery").
		Select("recovery.*").
		Joins("JOIN provider_accounts AS account ON account.id = recovery.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = account.QuotaRecovery{
			AccountID: row.AccountID, Kind: account.QuotaRecoveryKind(row.Kind), Status: account.QuotaRecoveryStatus(row.Status), ConfirmedUsed: row.ConfirmedUsed,
			ConfirmedLimit: row.ConfirmedLimit, ExhaustedAt: row.ExhaustedAt, NextProbeAt: row.NextProbeAt,
			LastConfirmedAt: row.LastConfirmedAt, UpdatedAt: row.UpdatedAt,
		}
	}
	return result, nil
}

var routingQuotaWindowColumns = []string{
	"account_id", "mode", "snapshot_version", "revision", "remaining", "total", "usage_percent", "window_seconds", "reset_at", "synced_at", "source", "updated_at",
}

func (r *AccountRepository) getRoutingQuotaWindows(ctx context.Context, provider account.Provider, quotaMode string, credentials []account.Credential, accountID uint64) (map[uint64]account.QuotaWindow, error) {
	result := make(map[uint64]account.QuotaWindow)
	if provider != account.ProviderWeb && quotaMode == "" {
		return result, nil
	}
	modes := make([]string, 0, 3)
	webImagineMode := provider == account.ProviderWeb && account.IsWebImagineQuotaMode(quotaMode)
	// Paid Web routes use the shared weekly pool. Imagine may additionally
	// expose a product-specific remainingQueries window (notably for Basic and
	// older response shapes); load both and prefer the exact product window
	// below, falling back to weekly only for confirmed Super/Heavy accounts.
	if provider == account.ProviderWeb {
		modes = append(modes, "weekly")
	}
	if provider == account.ProviderWeb && quotaMode == account.QuotaModeWebImageEdit {
		// Basic Web accounts use image_pro for editing, while Super/Heavy
		// accounts have the dedicated image_edit product. Load both once and
		// select the authoritative window per account below.
		modes = append(modes, account.QuotaModeWebImagePro)
	}
	if quotaMode != "" {
		modes = append(modes, quotaMode)
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).
		Scopes(routingAccountFilter("account.id", accountID)).
		Table("account_quota_windows AS quota").
		Select(qualifiedColumnList("quota", routingQuotaWindowColumns)).
		Joins("JOIN provider_accounts AS account ON account.id = quota.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND quota.mode IN ?", provider, true, account.AuthStatusActive, modes).
		Order("CASE WHEN quota.mode = 'weekly' THEN 0 ELSE 1 END").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	webTiers := make(map[uint64]account.WebTier, len(credentials))
	for _, credential := range credentials {
		webTiers[credential.ID] = credential.WebTier
	}
	for _, row := range rows {
		if webImagineMode {
			tier := webTiers[row.AccountID]
			productMode := quotaMode
			if quotaMode == account.QuotaModeWebImageEdit {
				productMode = webImageEditRoutingQuotaMode(tier)
			}
			switch {
			case row.Mode == productMode:
				// An explicit product window is more precise than the shared pool,
				// regardless of query order.
				result[row.AccountID] = toRoutingQuotaWindowDomain(row)
			case row.Mode == "weekly" && (tier == account.WebTierSuper || tier == account.WebTierHeavy):
				if existing, exists := result[row.AccountID]; !exists || existing.Mode != productMode {
					result[row.AccountID] = toRoutingQuotaWindowDomain(row)
				}
			}
			continue
		}
		if _, exists := result[row.AccountID]; !exists {
			result[row.AccountID] = toRoutingQuotaWindowDomain(row)
		}
	}
	return result, nil
}

func webImageEditRoutingQuotaMode(tier account.WebTier) string {
	switch tier {
	case account.WebTierSuper, account.WebTierHeavy:
		return account.QuotaModeWebImageEdit
	default:
		// Empty and auto tiers are deliberately treated as Basic, matching the
		// Web adapter's conservative capability normalization.
		return account.QuotaModeWebImagePro
	}
}

func (r *AccountRepository) ListRoutingAccountOverlays(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel string) (account.RoutingOverlaySnapshot, error) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return account.RoutingOverlaySnapshot{}, nil
	}
	boundIDs, err := r.listRoutingBoundAccountIDs(ctx, provider, modelRouteID, upstreamModel)
	if err != nil {
		return account.RoutingOverlaySnapshot{}, mapError(err)
	}
	values := make(map[uint64]account.RoutingAccountOverlay)
	for _, id := range boundIDs {
		values[id] = account.RoutingAccountOverlay{AccountID: id, Bound: true, ModelCapabilityKnown: true, SupportsModel: true}
	}
	var states []accountModelSyncStateModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_sync_states AS state").
		Select("state.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = state.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND state.last_success_at IS NOT NULL", provider).
		Find(&states).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, mapError(err)
	}
	for _, state := range states {
		overlay := values[state.AccountID]
		overlay.AccountID = state.AccountID
		overlay.ModelCapabilityKnown = true
		values[state.AccountID] = overlay
	}
	var capabilities []accountModelCapabilityModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_capabilities AS capability").
		Select("capability.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = capability.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND capability.upstream_model = ?", provider, upstreamModel).
		Find(&capabilities).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, mapError(err)
	}
	for _, capability := range capabilities {
		overlay := values[capability.AccountID]
		overlay.AccountID = capability.AccountID
		overlay.SupportsModel = true
		values[capability.AccountID] = overlay
	}
	var blockRows []accountModelQuotaBlockModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_quota_blocks AS block").
		Select("block.account_id", "block.upstream_model", "block.reason", "block.cooldown_until", "block.updated_at").
		Joins("JOIN provider_accounts AS account ON account.id = block.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND block.upstream_model = ? AND block.cooldown_until > ?", provider, upstreamModel, time.Now().UTC()).
		Find(&blockRows).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, mapError(err)
	}
	for _, row := range blockRows {
		overlay := values[row.AccountID]
		overlay.AccountID = row.AccountID
		block := modelRestrictionDomain(row)
		if overlay.ModelQuotaBlock != nil {
			block = account.DominantModelRestriction(*overlay.ModelQuotaBlock, block)
		}
		overlay.ModelQuotaBlock = &block
		values[row.AccountID] = overlay
	}
	result := account.RoutingOverlaySnapshot{HasBindings: len(boundIDs) > 0, Values: make([]account.RoutingAccountOverlay, 0, len(values))}
	for _, value := range values {
		result.Values = append(result.Values, value)
	}
	return result, nil
}

func (r *AccountRepository) listRoutingBoundAccountIDs(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel string) ([]uint64, error) {
	var accountIDs []uint64
	if err := r.routingBindingsQuery(ctx, provider, modelRouteID, upstreamModel).Scan(&accountIDs).Error; err != nil {
		return nil, err
	}
	return accountIDs, nil
}

func (r *AccountRepository) routingBindingsQuery(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel string) *gorm.DB {
	query := r.db.db.WithContext(ctx).
		Table("model_route_accounts AS binding").
		Select("binding.account_id").
		Joins("JOIN model_routes AS route ON route.id = binding.model_route_id")
	if modelRouteID > 0 {
		query = query.Where("route.id = ? AND route.provider = ? AND route.upstream_model = ?", modelRouteID, provider, upstreamModel)
	} else {
		query = query.Where("route.provider = ? AND route.upstream_model = ?", provider, upstreamModel)
	}
	return query
}

func (r *AccountRepository) ListEnabledAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error) {
	query := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive)
	if refreshableOnly {
		query = query.
			Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
			Where("credential.encrypted_refresh <> ''")
	}
	var ids []uint64
	err := query.Order("account.priority DESC, account.id ASC").Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) ListEnabledCredentialRefreshAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error) {
	query := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status IN ?", provider, true, []account.AuthStatus{account.AuthStatusActive, account.AuthStatusReauthRequired})
	if refreshableOnly {
		query = query.
			Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
			Where("credential.encrypted_refresh <> ''")
	}
	var ids []uint64
	err := query.Order("account.id ASC").Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) FilterMissingBuildConversionIDs(ctx context.Context, ids []uint64) ([]uint64, error) {
	if len(ids) == 0 {
		return []uint64{}, nil
	}
	var linkedIDs []uint64
	if err := r.db.db.WithContext(ctx).Model(&accountProviderLinkModel{}).
		Where("web_account_id IN ?", ids).Pluck("web_account_id", &linkedIDs).Error; err != nil {
		return nil, err
	}
	linked := make(map[uint64]struct{}, len(linkedIDs))
	for _, id := range linkedIDs {
		linked[id] = struct{}{}
	}
	values := make([]uint64, 0, len(ids)-len(linked))
	for _, id := range ids {
		if _, exists := linked[id]; !exists {
			values = append(values, id)
		}
	}
	return values, nil
}

func (r *AccountRepository) ListUnlinkedWebAccountIDs(ctx context.Context, afterID uint64, limit int) ([]uint64, int64, error) {
	if limit < 1 {
		return []uint64{}, 0, nil
	}
	query := func() *gorm.DB {
		return r.db.db.WithContext(ctx).
			Table("provider_accounts AS account").
			Joins("LEFT JOIN account_provider_links AS link ON link.web_account_id = account.id").
			Where("account.provider = ? AND link.web_account_id IS NULL", account.ProviderWeb)
	}
	var total int64
	if afterID == 0 {
		if err := query().Count(&total).Error; err != nil {
			return nil, 0, err
		}
	}
	var ids []uint64
	err := query().
		Select("account.id").
		Where("account.id > ?", afterID).
		Order("account.id ASC").
		Limit(limit).
		Scan(&ids).Error
	return ids, total, err
}

func (r *AccountRepository) ListMissingConsoleSyncAccounts(ctx context.Context, ids []uint64) ([]account.Credential, error) {
	if len(ids) == 0 {
		return []account.Credential{}, nil
	}
	var existing int64
	if err := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("id IN ? AND provider = ?", ids, account.ProviderWeb).Count(&existing).Error; err != nil {
		return nil, err
	}
	if existing != int64(len(ids)) {
		return nil, repository.ErrNotFound
	}
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).
		Preload("Credential").Preload("WebProfile").
		Where("id IN ? AND provider = ?", ids, account.ProviderWeb).
		Where(missingConsoleAccountPredicate, account.ProviderConsole).
		Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		values = append(values, toAccountDomain(row))
	}
	return values, nil
}

func (r *AccountRepository) ListMissingConsoleSyncBatch(ctx context.Context, afterID uint64, limit int) ([]account.Credential, int64, int64, error) {
	if limit < 1 {
		return []account.Credential{}, 0, 0, nil
	}
	query := func() *gorm.DB {
		return r.db.db.WithContext(ctx).Model(&accountModel{}).
			Where("provider = ?", account.ProviderWeb).
			Where(missingConsoleAccountPredicate, account.ProviderConsole)
	}
	var total, skipped int64
	if afterID == 0 {
		if err := query().Count(&total).Error; err != nil {
			return nil, 0, 0, err
		}
		var all int64
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("provider = ?", account.ProviderWeb).Count(&all).Error; err != nil {
			return nil, 0, 0, err
		}
		skipped = max(0, all-total)
	}
	var rows []accountModel
	if err := query().Preload("Credential").Preload("WebProfile").
		Where("id > ?", afterID).Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, 0, err
	}
	values := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		values = append(values, toAccountDomain(row))
	}
	return values, total, skipped, nil
}

// GetCredentialMaterial hydrates the encrypted provider data for one account
// after routing has selected it. Routing candidate queries intentionally never
// load these encrypted columns.
func (r *AccountRepository) GetCredentialMaterial(ctx context.Context, accountID uint64, provider account.Provider) (account.CredentialMaterial, error) {
	var row accountCredentialModel
	if err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.*").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("credential.account_id = ? AND account.provider = ? AND account.enabled = TRUE AND account.auth_status = ?", accountID, provider, account.AuthStatusActive).
		Take(&row).Error; err != nil {
		return account.CredentialMaterial{}, mapError(err)
	}
	return toCredentialMaterialDomain(row, provider), nil
}

func (r *AccountRepository) attachAccountLinks(ctx context.Context, values []account.Credential) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(values))
	positions := make(map[uint64]int, len(values))
	for index := range values {
		ids = append(ids, values[index].ID)
		positions[values[index].ID] = index
	}
	var buildRows []struct {
		WebAccountID            uint64
		BuildAccountID          uint64
		WebName                 string
		BuildName               string
		WebEmail                string
		BuildEmail              string
		WebUserID               string
		BuildUserID             string
		WebSourceKey            string
		EgressIdentity          string
		WebNSFWEnabledAt        *time.Time
		WebTermsAcceptedAt      *time.Time
		WebTermsAcceptedVersion int
	}
	err := r.db.db.WithContext(ctx).Table("account_provider_links AS link").
		Select("link.web_account_id, link.build_account_id, web.name AS web_name, build.name AS build_name, web.email AS web_email, build.email AS build_email, web.user_id AS web_user_id, build.user_id AS build_user_id, web.source_key AS web_source_key, profile.egress_identity, profile.nsfw_enabled_at AS web_nsfw_enabled_at, profile.terms_accepted_at AS web_terms_accepted_at, profile.terms_accepted_version AS web_terms_accepted_version").
		Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
		Joins("JOIN provider_accounts AS build ON build.id = link.build_account_id").
		Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
		Where("link.web_account_id IN ? OR link.build_account_id IN ?", ids, ids).
		Scan(&buildRows).Error
	if err != nil {
		return err
	}
	for _, row := range buildRows {
		egressIdentity := linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		if index, ok := positions[row.WebAccountID]; ok {
			values[index].LinkedAccountID = row.BuildAccountID
			values[index].LinkedAccountName = row.BuildName
			values[index].LinkedProvider = account.ProviderBuild
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.BuildAccountID, Provider: account.ProviderBuild, Name: row.BuildName, Email: row.BuildEmail, UserID: row.BuildUserID})
			if values[index].EgressIdentity == "" {
				values[index].EgressIdentity = egressIdentity
			}
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
		if index, ok := positions[row.BuildAccountID]; ok {
			values[index].LinkedAccountID = row.WebAccountID
			values[index].LinkedAccountName = row.WebName
			values[index].LinkedProvider = account.ProviderWeb
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.WebAccountID, Provider: account.ProviderWeb, Name: row.WebName, Email: row.WebEmail, UserID: row.WebUserID})
			values[index].EgressIdentity = egressIdentity
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
	}
	var consoleRows []struct {
		WebAccountID            uint64
		ConsoleAccountID        uint64
		WebName                 string
		ConsoleName             string
		WebEmail                string
		ConsoleEmail            string
		WebUserID               string
		ConsoleUserID           string
		WebSourceKey            string
		EgressIdentity          string
		WebNSFWEnabledAt        *time.Time
		WebTermsAcceptedAt      *time.Time
		WebTermsAcceptedVersion int
	}
	if err := r.db.db.WithContext(ctx).Table("web_console_account_links AS link").
		Select("link.web_account_id, link.console_account_id, web.name AS web_name, console.name AS console_name, web.email AS web_email, console.email AS console_email, web.user_id AS web_user_id, console.user_id AS console_user_id, web.source_key AS web_source_key, profile.egress_identity, profile.nsfw_enabled_at AS web_nsfw_enabled_at, profile.terms_accepted_at AS web_terms_accepted_at, profile.terms_accepted_version AS web_terms_accepted_version").
		Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
		Joins("JOIN provider_accounts AS console ON console.id = link.console_account_id").
		Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
		Where("link.web_account_id IN ? OR link.console_account_id IN ?", ids, ids).
		Scan(&consoleRows).Error; err != nil {
		return err
	}
	for _, row := range consoleRows {
		egressIdentity := linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		if index, ok := positions[row.WebAccountID]; ok {
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.ConsoleAccountID, Provider: account.ProviderConsole, Name: row.ConsoleName, Email: row.ConsoleEmail, UserID: row.ConsoleUserID})
			if values[index].EgressIdentity == "" {
				values[index].EgressIdentity = egressIdentity
			}
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
		if index, ok := positions[row.ConsoleAccountID]; ok {
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.WebAccountID, Provider: account.ProviderWeb, Name: row.WebName, Email: row.WebEmail, UserID: row.WebUserID})
			values[index].EgressIdentity = egressIdentity
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
	}
	return nil
}

func currentWebTermsAcceptedAt(value *time.Time, version int) *time.Time {
	if version < account.CurrentWebTermsVersion {
		return nil
	}
	return value
}

// attachRoutingEgressIdentities 只补充推理路由需要的稳定出口身份。
// 管理端展示所需的账号名称和 linkedAccounts 仍由 attachAccountLinks 加载，
// 避免路由候选缓存刷新时额外查询两类完整关系。
func (r *AccountRepository) attachRoutingEgressIdentities(ctx context.Context, provider account.Provider, values []account.Credential) error {
	if len(values) == 0 || provider == account.ProviderWeb {
		return nil
	}
	positions := make(map[uint64]int, len(values))
	for index := range values {
		positions[values[index].ID] = index
	}
	type identityRow struct {
		AccountID      uint64
		WebSourceKey   string
		EgressIdentity string
	}
	var rows []identityRow
	query := r.db.db.WithContext(ctx)
	switch provider {
	case account.ProviderBuild:
		query = query.Table("account_provider_links AS link").
			Select("link.build_account_id AS account_id, web.source_key AS web_source_key, profile.egress_identity").
			Joins("JOIN provider_accounts AS target ON target.id = link.build_account_id").
			Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
			Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
			Where("target.provider = ? AND target.enabled = ? AND target.auth_status = ?", provider, true, account.AuthStatusActive)
	case account.ProviderConsole:
		query = query.Table("web_console_account_links AS link").
			Select("link.console_account_id AS account_id, web.source_key AS web_source_key, profile.egress_identity").
			Joins("JOIN provider_accounts AS target ON target.id = link.console_account_id").
			Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
			Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
			Where("target.provider = ? AND target.enabled = ? AND target.auth_status = ?", provider, true, account.AuthStatusActive)
	default:
		return nil
	}
	if err := query.Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if index, ok := positions[row.AccountID]; ok {
			values[index].EgressIdentity = linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		}
	}
	return nil
}

func linkedWebEgressIdentity(stored, sourceKey string) string {
	if value := strings.TrimSpace(stored); value != "" {
		return value
	}
	value, _ := egressIdentityFromWebSourceKey(sourceKey)
	return value
}

// UpsertByIdentity is the single direct-import convenience entry. It uses the
// same current deletion policy as ImportAccounts; a skipped import is a conflict.
func (r *AccountRepository) UpsertByIdentity(ctx context.Context, value account.Credential) (account.Credential, bool, error) {
	results, err := r.ImportAccounts(ctx, []repository.AccountImport{{Credential: value}})
	if err != nil {
		return account.Credential{}, false, err
	}
	result := results[0]
	if result.Skipped != "" {
		return account.Credential{}, false, fmt.Errorf("%w: 账号已删除，无法重新导入", repository.ErrConflict)
	}
	stored, err := r.Get(ctx, result.ID)
	return stored, result.Created, err
}

// UpsertManyByIdentity imports material without an existing source account.
func (r *AccountRepository) ImportAccounts(ctx context.Context, inputs []repository.AccountImport) ([]repository.AccountUpsertResult, error) {
	if len(inputs) == 0 {
		return []repository.AccountUpsertResult{}, nil
	}
	values := make([]account.Credential, len(inputs))
	for i, input := range inputs {
		values[i] = input.Credential
	}
	results := make([]repository.AccountUpsertResult, len(inputs))
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountImport(tx); err != nil {
			return err
		}
		identityKeys := make([]string, 0, len(values))
		sourceKeysByProvider := make(map[account.Provider][]string)
		for _, value := range values {
			identityKeys = append(identityKeys, fromAccountDomain(value).IdentityKey)
			if strings.TrimSpace(value.SourceKey) != "" {
				sourceKeysByProvider[value.Provider] = append(sourceKeysByProvider[value.Provider], value.SourceKey)
			}
		}
		var existingRows []accountModel
		if err := tx.Where("identity_key IN ?", identityKeys).Find(&existingRows).Error; err != nil {
			return err
		}
		existingByIdentity := make(map[string]accountModel, len(values))
		for _, row := range existingRows {
			existingByIdentity[row.IdentityKey] = row
		}
		existingBySource := make(map[string]accountModel, len(values))
		for providerValue, sourceKeys := range sourceKeysByProvider {
			var sourceRows []accountModel
			if err := tx.Where("provider = ? AND source_key IN ?", providerValue, sourceKeys).Find(&sourceRows).Error; err != nil {
				return err
			}
			for _, row := range sourceRows {
				key := providerSourceLookupKey(row.Provider, row.SourceKey)
				if existing, duplicate := existingBySource[key]; duplicate && existing.ID != row.ID {
					return fmt.Errorf("Provider %s 的来源凭据匹配多个账号", row.Provider)
				}
				existingBySource[key] = row
			}
		}
		// Lock all known target/source rows in one order before taking any
		// credential/FK locks. Source material and current email are then stable
		// against ordinary credential and identity updates too.
		current, generations, tombstoned, err := lockAccountImportFacts(tx, inputs, existingByIdentity, existingBySource)
		if err != nil {
			return err
		}
		for index, value := range values {
			identityKey := fromAccountDomain(value).IdentityKey
			existing, foundByIdentity := existingByIdentity[identityKey]
			bySource, foundBySource := existingBySource[providerSourceLookupKey(string(value.Provider), value.SourceKey)]
			if foundByIdentity && foundBySource && existing.ID != bySource.ID {
				return fmt.Errorf("账号身份与来源凭据指向不同账号")
			}
			if !foundByIdentity && foundBySource {
				existing = bySource
			}
			var target *accountModel
			if foundByIdentity || foundBySource {
				if locked, ok := current[existing.ID]; ok {
					existing = locked
				}
				target = &existing
			}
			if reason := accountImportSkip(inputs[index], target, current, generations, tombstoned); reason != "" {
				results[index].Skipped = reason
				continue
			}
			result, stored, err := upsertKnownAccountByIdentity(tx, value, target)
			if err != nil {
				return err
			}
			results[index] = result
			current[stored.ID] = stored
			generations[stored.ID]++
			existingByIdentity[stored.IdentityKey] = stored
			existingBySource[providerSourceLookupKey(stored.Provider, stored.SourceKey)] = stored
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}
	providers := make(map[account.Provider]struct{})
	for i, value := range values {
		if results[i].Skipped == "" {
			providers[value.Provider] = struct{}{}
		}
	}
	for providerValue := range providers {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: providerValue})
	}
	return results, nil
}

func providerSourceLookupKey(providerValue, sourceKey string) string {
	return providerValue + "\x00" + sourceKey
}

func upsertKnownAccountByIdentity(tx *gorm.DB, value account.Credential, existing *accountModel) (repository.AccountUpsertResult, accountModel, error) {
	row := fromAccountDomain(value)
	row.QuotaRecoveryRevision, row.QuotaRecoveryResetRevision = 0, 0
	if existing != nil {
		// A batch may have loaded this row before another connection committed
		// health. Lock and reread before preserving mutable account state.
		if err := lockProviderAccount(tx, existing.ID, value.Provider); err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		var current accountModel
		if err := tx.First(&current, existing.ID).Error; err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		existing = &current
		var storedCredential accountCredentialModel
		if err := tx.Where("account_id = ?", existing.ID).First(&storedCredential).Error; err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		if storedCredential.Generation >= math.MaxInt64 {
			return repository.AccountUpsertResult{}, accountModel{}, account.ErrCredentialGenerationExhausted
		}
		value.CredentialGeneration = storedCredential.Generation + 1
		if value.EncryptedCloudflareCookie == "" {
			value.EncryptedCloudflareCookie = storedCredential.EncryptedCloudflareCookie
		}

		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
		row.Enabled = existing.Enabled
		// risk_status 是长期风控标记：重新导入/令牌同步等 upsert 路径不得
		// 用传入的新实体（无标记）清空它（外部复核发现）。
		row.RiskStatus = existing.RiskStatus
		row.RiskTrigger = existing.RiskTrigger
		row.RiskOriginAccountID = existing.RiskOriginAccountID
		row.RiskCheckedAt = existing.RiskCheckedAt
		row.RiskDetail = existing.RiskDetail
		row.Priority = existing.Priority
		row.MaxConcurrent = existing.MaxConcurrent
		row.MinimumRemaining = existing.MinimumRemaining
		row.FailureCount = existing.FailureCount
		row.HealthRevision = existing.HealthRevision
		row.QuotaRecoveryRevision, row.QuotaRecoveryResetRevision = existing.QuotaRecoveryRevision, existing.QuotaRecoveryResetRevision
		row.CooldownMarkedAt = existing.CooldownMarkedAt
		row.CooldownUntil = existing.CooldownUntil
		row.LastError = existing.LastError
		row.LastUsedAt = existing.LastUsedAt
		row.ObservedModel = existing.ObservedModel
		row.ObservedModelAt = existing.ObservedModelAt
		// 账号级 Build 路由、XAI 回退记录与 Super entitlement 在 upsert/转换/刷新路径中保留。
		row.BuildAPIFallback = existing.BuildAPIFallback
		row.BuildRouteMode = existing.BuildRouteMode
		row.BuildSuperEntitled = existing.BuildSuperEntitled
		// 与认证事件一致：保持 reauth 时沿用锚点，导入 active 时清空。
		applyReauthMarkedAtTransition(&row, *existing)
		if err := tx.Save(&row).Error; err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		if err := resetWebProfileForChangedIdentity(tx, value.Provider, row.ID, existing.UserID, row.UserID); err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		if err := saveAccountRelations(tx, value, row.ID); err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		return repository.AccountUpsertResult{ID: row.ID, Material: account.CredentialRef{AccountID: row.ID, Provider: value.Provider, Generation: value.CredentialGeneration}}, row, nil
	}
	value.CredentialGeneration = 1
	if row.AuthStatus == "" {
		row.AuthStatus = string(account.AuthStatusActive)
	}
	if row.AuthStatus == string(account.AuthStatusReauthRequired) && row.ReauthMarkedAt == nil {
		now := time.Now().UTC()
		row.ReauthMarkedAt = &now
	}
	if row.AuthStatus != string(account.AuthStatusReauthRequired) {
		row.ReauthMarkedAt = nil
	}
	if row.Priority == 0 {
		row.Priority = account.DefaultPriority
	}
	if row.MaxConcurrent == 0 {
		row.MaxConcurrent = account.DefaultMaxConcurrent
	}
	row.Enabled = true
	if err := tx.Create(&row).Error; err != nil {
		return repository.AccountUpsertResult{}, accountModel{}, err
	}
	if err := saveAccountRelations(tx, value, row.ID); err != nil {
		return repository.AccountUpsertResult{}, accountModel{}, err
	}
	return repository.AccountUpsertResult{ID: row.ID, Created: true, Material: account.CredentialRef{AccountID: row.ID, Provider: value.Provider, Generation: value.CredentialGeneration}}, row, nil
}

// applyReauthMarkedAtTransition 仅在状态切入 reauthRequired 时打锚点；保持 reauth 时保留原锚点；离开 reauth 时清空。
func applyReauthMarkedAtTransition(row *accountModel, existing accountModel) {
	if row.AuthStatus == string(account.AuthStatusReauthRequired) {
		if existing.AuthStatus == string(account.AuthStatusReauthRequired) && existing.ReauthMarkedAt != nil {
			row.ReauthMarkedAt = existing.ReauthMarkedAt
			return
		}
		if row.ReauthMarkedAt == nil {
			now := time.Now().UTC()
			row.ReauthMarkedAt = &now
		}
		return
	}
	row.ReauthMarkedAt = nil
}

// saveAccountRelations installs the material for an explicit import.
func saveAccountRelations(tx *gorm.DB, value account.Credential, accountID uint64) error {
	value.ID = accountID
	credential := fromAccountCredentialDomain(value)
	if err := tx.Save(&credential).Error; err != nil {
		return err
	}
	if profile := fromWebProfileDomain(value); profile != nil {
		updates := []string{"tier", "synced_at"}
		if profile.NSFWEnabledAt != nil {
			updates = append(updates, "nsfw_enabled_at")
		}
		if profile.TermsAcceptedAt != nil {
			updates = append(updates, "terms_accepted_at")
		}
		if profile.TermsAcceptedVersion > 0 {
			updates = append(updates, "terms_accepted_version")
		}
		if profile.BirthDateSetAt != nil {
			updates = append(updates, "birth_date_set_at")
		}
		if strings.TrimSpace(profile.EgressIdentity) != "" {
			updates = append(updates, "egress_identity")
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}},
			DoUpdates: clause.AssignmentColumns(updates),
		}).Create(profile).Error
	}
	return tx.Where("account_id = ?", accountID).Delete(&webAccountProfileModel{}).Error
}

func (r *AccountRepository) UpdateMany(ctx context.Context, providerValue account.Provider, ids []uint64, updates repository.AccountUpdates) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	values := make(map[string]any, 4)
	if updates.Enabled != nil {
		values["enabled"] = *updates.Enabled
	}
	if updates.Priority != nil {
		values["priority"] = *updates.Priority
	}
	if updates.MaxConcurrent != nil {
		values["max_concurrent"] = *updates.MaxConcurrent
	}
	if updates.MinimumRemaining != nil {
		values["minimum_remaining"] = *updates.MinimumRemaining
	}
	if len(values) == 0 {
		return 0, nil
	}
	var updated int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(ids); start += accountUpdateBatchSize {
			end := min(start+accountUpdateBatchSize, len(ids))
			var rows []accountModel
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "provider").Where("id IN ?", ids[start:end]).Order("id ASC").Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) != end-start {
				return repository.ErrAccountPoolMismatch
			}
			for _, row := range rows {
				if account.Provider(row.Provider) != providerValue {
					return repository.ErrAccountPoolMismatch
				}
			}
		}
		for start := 0; start < len(ids); start += accountUpdateBatchSize {
			end := min(start+accountUpdateBatchSize, len(ids))
			result := tx.Model(&accountModel{}).Where("provider = ? AND id IN ?", providerValue, ids[start:end]).Updates(values)
			if result.Error != nil {
				return result.Error
			}
			updated += result.RowsAffected
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if updated > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return updated, nil
}

func (r *AccountRepository) Delete(ctx context.Context, id uint64) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}
		var lockedID uint64
		if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Pluck("id", &lockedID).Error; err != nil {
			return err
		}
		if lockedID == 0 {
			return repository.ErrNotFound
		}
		if err := rejectAccountsWithMediaJobs(tx, []uint64{id}); err != nil {
			return err
		}
		if err := writeDeletedAccountTombstones(tx, []uint64{id}); err != nil {
			return err
		}
		return mapError(tx.Delete(&accountModel{}, id).Error)
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return err
}

func (r *AccountRepository) DeleteMany(ctx context.Context, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}
		var lockedIDs []uint64
		if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Pluck("id", &lockedIDs).Error; err != nil {
			return err
		}
		if err := rejectAccountsWithMediaJobs(tx, lockedIDs); err != nil {
			return err
		}
		if err := writeDeletedAccountTombstones(tx, lockedIDs); err != nil {
			return err
		}
		result := tx.Where("id IN ?", lockedIDs).Delete(&accountModel{})
		deleted = result.RowsAffected
		return result.Error
	})
	if err == nil && deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return deleted, err
}

// excludeAccountsWithActiveMediaJobs 返回无 queued/in_progress 视频任务的账号 ID（顺序保持输入顺序）。
func excludeAccountsWithActiveMediaJobs(db *gorm.DB, ids []uint64) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var blocked []uint64
	if err := db.Model(&mediaJobModel{}).
		Distinct("account_id").
		Where("account_id IN ? AND status IN ?", ids, []string{string(media.StatusQueued), string(media.StatusInProgress)}).
		Pluck("account_id", &blocked).Error; err != nil {
		return nil, err
	}
	if len(blocked) == 0 {
		out := make([]uint64, len(ids))
		copy(out, ids)
		return out, nil
	}
	blockedSet := make(map[uint64]struct{}, len(blocked))
	for _, id := range blocked {
		blockedSet[id] = struct{}{}
	}
	out := make([]uint64, 0, len(ids)-len(blocked))
	for _, id := range ids {
		if _, skip := blockedSet[id]; skip {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// activeMediaJobStatuses lists video states that still require the account and block deletion.
func activeMediaJobStatuses() []string {
	return []string{string(media.StatusQueued), string(media.StatusInProgress)}
}

// rejectAccountsWithMediaJobs 仅保护仍需账号继续执行的活动视频任务。
// completed/failed 已保存账号名称等快照，删除账号后由外键 SET NULL 保留历史。
func rejectAccountsWithMediaJobs(db *gorm.DB, ids []uint64) error {
	var count int64
	if err := db.Model(&mediaJobModel{}).
		Where("account_id IN ? AND status IN ?", ids, activeMediaJobStatuses()).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: 账号仍关联 %d 条排队中或进行中的视频任务，请等待任务结束后重试", repository.ErrConflict, count)
	}
	return nil
}

func applyAccountStatusFilter(query *gorm.DB, status string, now time.Time) *gorm.DB {
	switch status {
	case "active":
		return query.Where("enabled = ? AND auth_status = ? AND NOT "+accountRecoveryPredicate+" AND NOT "+providerQuotaExhaustedPredicate+" AND risk_status = '' AND (cooldown_until IS NULL OR cooldown_until <= ?)", true, account.AuthStatusActive, now)
	case "risk":
		return query.Where("risk_status <> ''")
	case "disabled":
		return query.Where("enabled = ?", false)
	case "reauthRequired":
		return query.Where("enabled = ? AND auth_status = ?", true, account.AuthStatusReauthRequired)
	case "cooldown":
		return query.Where("enabled = ? AND auth_status = ? AND NOT "+accountRecoveryPredicate+" AND cooldown_until > ?", true, account.AuthStatusActive, now)
	case "waitingReset":
		return query.Where("enabled = ? AND auth_status = ? AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR "+providerQuotaExhaustedPredicate+")", true, account.AuthStatusActive)
	case "probing":
		return query.Where("enabled = ? AND auth_status = ? AND EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing')", true, account.AuthStatusActive)
	default:
		return query
	}
}

// Web agreement predicates match the effective state exposed by the admin API.
// Terms are current only when the recorded version reaches CurrentWebTermsVersion.
const (
	webNSFWEnabledPredicate   = "EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id AND profile.nsfw_enabled_at IS NOT NULL)"
	webTermsAcceptedPredicate = "EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id AND profile.terms_accepted_at IS NOT NULL AND profile.terms_accepted_version >= ?)"
	webBuildLinkedPredicate   = "EXISTS (SELECT 1 FROM account_provider_links link WHERE link.web_account_id = provider_accounts.id)"
	webConsoleLinkedPredicate = "EXISTS (SELECT 1 FROM web_console_account_links link WHERE link.web_account_id = provider_accounts.id)"
	// Build and Console filter by whether a Web link exists.
	buildWebLinkedPredicate   = "EXISTS (SELECT 1 FROM account_provider_links link WHERE link.build_account_id = provider_accounts.id)"
	consoleWebLinkedPredicate = "EXISTS (SELECT 1 FROM web_console_account_links link WHERE link.console_account_id = provider_accounts.id)"
)

func applyWebAgreementFilter(query *gorm.DB, agreement string) *gorm.DB {
	switch agreement {
	case "nsfwEnabled":
		return query.Where(webNSFWEnabledPredicate)
	case "nsfwDisabled":
		return query.Where("NOT " + webNSFWEnabledPredicate)
	case "termsAccepted":
		return query.Where(webTermsAcceptedPredicate, account.CurrentWebTermsVersion)
	case "termsNotAccepted":
		return query.Where("NOT "+webTermsAcceptedPredicate, account.CurrentWebTermsVersion)
	case "allAccepted":
		return query.Where(webNSFWEnabledPredicate).Where(webTermsAcceptedPredicate, account.CurrentWebTermsVersion)
	case "allNotAccepted":
		return query.Where("NOT "+webNSFWEnabledPredicate).Where("NOT "+webTermsAcceptedPredicate, account.CurrentWebTermsVersion)
	default:
		return query
	}
}

// applyAssociationFilter applies provider-specific association predicates.
// Web supports Build, Console, and combined filters; Build and Console use
// provider-specific foreign keys for webLinked and webUnlinked.
func applyAssociationFilter(query *gorm.DB, providerValue, association string) *gorm.DB {
	switch association {
	case "buildLinked":
		return query.Where(webBuildLinkedPredicate)
	case "buildUnlinked":
		return query.Where("NOT " + webBuildLinkedPredicate)
	case "consoleLinked":
		return query.Where(webConsoleLinkedPredicate)
	case "consoleUnlinked":
		return query.Where("NOT " + webConsoleLinkedPredicate)
	case "allLinked":
		return query.Where(webBuildLinkedPredicate).Where(webConsoleLinkedPredicate)
	case "allUnlinked":
		return query.Where("NOT " + webBuildLinkedPredicate).Where("NOT " + webConsoleLinkedPredicate)
	case "webLinked":
		if providerValue == string(account.ProviderConsole) {
			return query.Where(consoleWebLinkedPredicate)
		}
		return query.Where(buildWebLinkedPredicate)
	case "webUnlinked":
		if providerValue == string(account.ProviderConsole) {
			return query.Where("NOT " + consoleWebLinkedPredicate)
		}
		return query.Where("NOT " + buildWebLinkedPredicate)
	default:
		return query
	}
}

// BackfillCredentialRefreshSchedules 为升级前凭据分批补齐调度时间，不解密 Token，也不发起 OAuth 请求。
func (r *AccountRepository) BackfillCredentialRefreshSchedules(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	var rows []struct {
		AccountID        uint64
		ExpiresAt        *time.Time
		EncryptedPrimary string
	}
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id, credential.expires_at, credential.encrypted_primary").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderBuild, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NULL", account.AuthTypeOAuth).
		Where("credential.expires_at IS NOT NULL OR credential.encrypted_primary = ''").
		Order("credential.account_id ASC").Limit(limit).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	err = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			dueAt := now
			if row.EncryptedPrimary != "" && row.ExpiresAt != nil && !row.ExpiresAt.IsZero() {
				dueAt = account.CredentialRefreshDueAt(row.AccountID, *row.ExpiresAt)
			}
			if err := tx.Model(&accountCredentialModel{}).Where("account_id = ? AND refresh_due_at IS NULL", row.AccountID).Update("refresh_due_at", dueAt).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return len(rows), err
}

// ListCriticalCredentialRefreshIDs 只返回重启后必须优先恢复的凭据，避免启动时刷新整个账号池。
func (r *AccountRepository) ListCriticalCredentialRefreshIDs(ctx context.Context, now, expiresBefore time.Time, limit int) ([]uint64, error) {
	if limit < 1 {
		return []uint64{}, nil
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderBuild, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> ''", account.AuthTypeOAuth).
		Where("credential.encrypted_primary = '' OR credential.expires_at <= ? OR (credential.refresh_failures > 0 AND credential.refresh_due_at IS NOT NULL AND credential.refresh_due_at <= ?)", expiresBefore.UTC(), now.UTC()).
		Order(gorm.Expr("CASE WHEN credential.encrypted_primary = '' THEN 0 WHEN credential.expires_at <= ? THEN 1 ELSE 2 END, credential.expires_at ASC, credential.account_id ASC", now.UTC())).
		Limit(limit).
		Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) ListDueCredentialRefreshIDs(ctx context.Context, now time.Time, limit int) ([]uint64, error) {
	if limit < 1 {
		return []uint64{}, nil
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderBuild, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NOT NULL AND credential.refresh_due_at <= ?", account.AuthTypeOAuth, now).
		Order("credential.refresh_due_at ASC, credential.account_id ASC").Limit(limit).Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) NextCredentialRefreshDueAt(ctx context.Context) (*time.Time, error) {
	var rows []struct{ RefreshDueAt time.Time }
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.refresh_due_at").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderBuild, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NOT NULL", account.AuthTypeOAuth).
		Order("credential.refresh_due_at ASC, credential.account_id ASC").Limit(1).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	value := rows[0].RefreshDueAt.UTC()
	return &value, nil
}

func (r *AccountRepository) UpdateObservedModel(ctx context.Context, id uint64, model string, observedAt time.Time) error {
	_, err := r.UpdateObservedModelIfNewer(ctx, id, model, observedAt)
	return err
}

func (r *AccountRepository) UpdateObservedModelIfNewer(ctx context.Context, id uint64, model string, observedAt time.Time) (bool, error) {
	model = truncate(model, 255)
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("id = ? AND (observed_model_at IS NULL OR observed_model_at <= ?) AND (COALESCE(observed_model, '') <> ? OR observed_model_at <= ?)", id, observedAt, model, observedAt.Add(-30*time.Minute)).
		Updates(map[string]any{"observed_model": model, "observed_model_at": observedAt})
	if result.Error == nil && result.RowsAffected > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, AccountID: id})
	}
	return result.RowsAffected > 0, result.Error
}

// MarkBuildAPIFallback idempotently updates the XAI inference fallback marker for Grok Build accounts.
func (r *AccountRepository) MarkBuildAPIFallback(ctx context.Context, id uint64, enabled bool) error {
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("id = ? AND provider = ?", id, account.ProviderBuild).
		Update("build_api_fallback", enabled)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return repository.ErrNotFound
		}
		return fmt.Errorf("仅 grok_build 账号支持 Build API 降级标记")
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild, AccountID: id})
	return nil
}

func riskAttributionFields(attr repository.RiskAttribution) map[string]any {
	fields := map[string]any{
		"risk_status":            attr.Status,
		"risk_trigger":           attr.Trigger,
		"risk_origin_account_id": attr.OriginAccountID,
		"risk_checked_at":        attr.CheckedAt,
		"risk_detail":            attr.Detail,
	}
	if attr.Status == "" {
		fields["risk_trigger"] = ""
		fields["risk_origin_account_id"] = 0
		fields["risk_checked_at"] = nil
		fields["risk_detail"] = ""
	}
	return fields
}

func (r *AccountRepository) TouchLastUsed(ctx context.Context, id uint64, usedAt time.Time) error {
	if id == 0 || usedAt.IsZero() {
		return repository.ErrNotFound
	}
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", id).Update("last_used_at", usedAt.UTC())
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *AccountRepository) PruneExpiredModelQuotaBlocks(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []accountModelQuotaBlockModel
	if err := r.db.db.WithContext(ctx).Select("account_id", "upstream_model", "reason").Where("cooldown_until <= ?", now.UTC()).Order("cooldown_until ASC").Limit(limit).Find(&rows).Error; err != nil || len(rows) == 0 {
		return 0, err
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			result := tx.Where("account_id = ? AND upstream_model = ? AND reason = ? AND cooldown_until <= ?", row.AccountID, row.UpstreamModel, row.Reason, now.UTC()).Delete(&accountModelQuotaBlockModel{})
			if result.Error != nil {
				return result.Error
			}
			deleted += result.RowsAffected
		}
		return nil
	})
	if err == nil && deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged})
	}
	return deleted, err
}

func saveBilling(tx *gorm.DB, value account.Billing) error {
	history, err := json.Marshal(value.History)
	if err != nil {
		return err
	}
	row := billingModel{AccountID: value.AccountID, PlanCode: truncate(value.PlanCode, 100), PlanName: truncate(value.PlanName, 160), MonthlyLimit: value.MonthlyLimit, Used: value.Used, OnDemandCap: value.OnDemandCap, OnDemandUsed: value.OnDemandUsed, PrepaidBalance: value.PrepaidBalance, CreditUsagePercent: value.CreditUsagePercent, IsUnifiedBillingUser: value.IsUnifiedBillingUser, OnDemandEnabled: value.OnDemandEnabled, TopUpMethod: truncate(value.TopUpMethod, 100), UsagePeriodType: truncate(value.UsagePeriodType, 100), UsagePeriodStart: truncate(value.UsagePeriodStart, 64), UsagePeriodEnd: truncate(value.UsagePeriodEnd, 64), BillingPeriodStart: truncate(value.BillingPeriodStart, 64), BillingPeriodEnd: truncate(value.BillingPeriodEnd, 64), HistoryJSON: string(history), SyncedAt: value.SyncedAt}
	return tx.Save(&row).Error
}

func (r *AccountRepository) GetQuotaRecovery(ctx context.Context, accountID uint64) (account.QuotaRecovery, error) {
	var row quotaRecoveryModel
	if err := r.db.db.WithContext(ctx).First(&row, "account_id = ?", accountID).Error; err != nil {
		return account.QuotaRecovery{}, mapError(err)
	}
	return account.QuotaRecovery{
		AccountID: row.AccountID, Kind: account.QuotaRecoveryKind(row.Kind), Status: account.QuotaRecoveryStatus(row.Status), ConfirmedUsed: row.ConfirmedUsed,
		ConfirmedLimit: row.ConfirmedLimit, ExhaustedAt: row.ExhaustedAt, NextProbeAt: row.NextProbeAt,
		LastConfirmedAt: row.LastConfirmedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *AccountRepository) GetQuotaRecoveries(ctx context.Context, accountIDs []uint64) (map[uint64]account.QuotaRecovery, error) {
	result := make(map[uint64]account.QuotaRecovery, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []quotaRecoveryModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = account.QuotaRecovery{
			AccountID: row.AccountID, Kind: account.QuotaRecoveryKind(row.Kind), Status: account.QuotaRecoveryStatus(row.Status), ConfirmedUsed: row.ConfirmedUsed,
			ConfirmedLimit: row.ConfirmedLimit, ExhaustedAt: row.ExhaustedAt, NextProbeAt: row.NextProbeAt,
			LastConfirmedAt: row.LastConfirmedAt, UpdatedAt: row.UpdatedAt,
		}
	}
	return result, nil
}

func (r *AccountRepository) ResetQuotaState(ctx context.Context, provider account.Provider, accountIDs []uint64) error {
	if len(accountIDs) == 0 {
		return nil
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		accountQuery := func() *gorm.DB {
			return tx.Model(&accountModel{}).Where("provider = ? AND id IN ?", provider, accountIDs)
		}
		if err := advanceQuotaReset(accountQuery); err != nil {
			return err
		}
		if err := tx.Where("account_id IN (?)", accountQuery().Select("id")).Delete(&quotaRecoveryModel{}).Error; err != nil {
			return err
		}
		return tx.Where("account_id IN (?) AND reason = ?", accountQuery().Select("id"), "model_quota_depleted").Delete(&accountModelQuotaBlockModel{}).Error
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, Provider: provider})
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged, Provider: provider})
	}
	return err
}

func (r *AccountRepository) ResetProviderQuotaState(ctx context.Context, provider account.Provider, activeOnly bool) (int64, error) {
	var accountCount int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		accountQuery := func() *gorm.DB {
			query := tx.Model(&accountModel{}).Where("provider = ?", provider)
			if activeOnly {
				query = query.Where("enabled = ? AND auth_status = ?", true, account.AuthStatusActive)
			}
			return query
		}
		if err := accountQuery().Count(&accountCount).Error; err != nil {
			return err
		}
		if err := advanceQuotaReset(accountQuery); err != nil {
			return err
		}
		if err := tx.Where("account_id IN (?)", accountQuery().Select("id")).Delete(&quotaRecoveryModel{}).Error; err != nil {
			return err
		}
		return tx.Where("account_id IN (?) AND reason = ?", accountQuery().Select("id"), "model_quota_depleted").Delete(&accountModelQuotaBlockModel{}).Error
		// PostgreSQL needs a repeatable snapshot so concurrent enable/auth edits
		// cannot change the cohort between count and the two deletes. SQLite's
		// BEGIN IMMEDIATE already serializes writers. No full ID set is allocated.
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err == nil && accountCount > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, Provider: provider})
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged, Provider: provider})
	}
	return accountCount, err
}

func (r *AccountRepository) HasQuotaWindows(ctx context.Context, accountID uint64) (bool, error) {
	var providerRow struct {
		Provider string
	}
	if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Select("provider").Where("id = ?", accountID).Take(&providerRow).Error; err != nil {
		return false, err
	}
	var count int64
	query := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).Where("account_id = ? AND synced_at IS NOT NULL", accountID)
	if account.Provider(providerRow.Provider) == account.ProviderConsole {
		// Pre-usage Console releases stored one synthetic local chat window.
		// Only the complete authoritative /usage snapshot counts as initialized,
		// so re-import and startup migration replace that legacy state.
		query = query.Where("source = ? AND mode IN ?", account.QuotaSourceUpstream, []string{"console", "console_image", "console_video"}).Distinct("mode")
		if err := query.Count(&count).Error; err != nil {
			return false, err
		}
		return count == 3, nil
	}
	if err := query.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *AccountRepository) GetQuotaWindows(ctx context.Context, accountIDs []uint64) (map[uint64][]account.QuotaWindow, error) {
	result := make(map[uint64][]account.QuotaWindow, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Order("account_id ASC, mode ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = append(result[row.AccountID], toQuotaWindowDomain(row))
	}
	return result, nil
}

func writeQuotaWindows(tx *gorm.DB, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow, replace bool, replaceModes []string, version uint64) error {
	if tier != "" {
		profile := webAccountProfileModel{AccountID: accountID, Tier: string(tier), SyncedAt: &syncedAt}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.AssignmentColumns([]string{"tier", "synced_at"})}).Create(&profile).Error; err != nil {
			return err
		}
	}
	if replace {
		if err := tx.Where("account_id = ?", accountID).Delete(&quotaWindowModel{}).Error; err != nil {
			return err
		}
	} else if len(replaceModes) > 0 {
		if err := tx.Where("account_id = ? AND mode IN ?", accountID, replaceModes).Delete(&quotaWindowModel{}).Error; err != nil {
			return err
		}
	}
	for _, value := range values {
		serializedBreakdown := make([]quotaBreakdownJSON, 0, len(value.Breakdown))
		for _, item := range value.Breakdown {
			serializedBreakdown = append(serializedBreakdown, quotaBreakdownJSON{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
		}
		breakdownJSON, err := json.Marshal(serializedBreakdown)
		if err != nil {
			return err
		}
		row := quotaWindowModel{
			AccountID: accountID, SnapshotVersion: version, Revision: version, Mode: truncate(strings.TrimSpace(value.Mode), 64), Remaining: max(0, value.Remaining), Total: max(0, value.Total),
			UsagePercent: min(100, max(0, value.UsagePercent)), BreakdownJSON: string(breakdownJSON),
			WindowSeconds: max(0, value.WindowSeconds), ResetAt: value.ResetAt, SyncedAt: value.SyncedAt, Source: string(value.Source), UpdatedAt: syncedAt,
		}
		if row.Source == "" {
			row.Source = string(account.QuotaSourceUpstream)
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}, {Name: "mode"}},
			DoUpdates: clause.AssignmentColumns([]string{"snapshot_version", "revision", "remaining", "total", "usage_percent", "breakdown_json", "window_seconds", "reset_at", "synced_at", "source", "updated_at"}),
		}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func (r *AccountRepository) ExhaustQuotaWindow(ctx context.Context, accountID uint64, mode string, resetAt *time.Time, now time.Time) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		exists, err := lockQuotaState(tx, accountID)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		result := tx.Model(&quotaWindowModel{}).Where("account_id = ? AND mode = ?", accountID, mode).
			Updates(map[string]any{"remaining": 0, "reset_at": resetAt, "updated_at": now, "revision": gorm.Expr("(SELECT revision + 1 FROM account_quota_state WHERE account_id = ?)", accountID)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			return bumpQuotaRevision(tx, accountID)
		}
		return nil
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: accountID})
	}
	return err
}

// ListDueQuotaWindows 返回"已到期"窗口的原始行。SQL 只表达粗略的到期上界
// （已耗尽且 reset_at 已过），是否值得安排恢复由调用方按 domain owner 的
// QuotaWindowControlsRouting 判定——这里不重复资格规则。
// Provider 随行返回，跨账号查询的调用方无法从窗口本身推导它。
func (r *AccountRepository) ListDueQuotaWindows(ctx context.Context, now time.Time, input repository.DueQuotaWindowQuery) ([]account.QuotaWindow, error) {
	limit := input.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []struct {
		Window   quotaWindowModel `gorm:"embedded"`
		Provider string
	}
	query := r.db.db.WithContext(ctx).Table("account_quota_windows AS w").
		Select("w.*, a.provider").Joins("JOIN provider_accounts AS a ON a.id = w.account_id").
		Where("w.remaining = 0 AND w.reset_at IS NOT NULL AND w.reset_at <= ?", now)
	if input.Provider != "" {
		query = query.Where("a.provider = ?", input.Provider)
	}
	if after := input.After; after != nil {
		query = query.Where("w.reset_at > ? OR (w.reset_at = ? AND (w.account_id > ? OR (w.account_id = ? AND w.mode > ?)))",
			after.ResetAt, after.ResetAt, after.AccountID, after.AccountID, after.Mode)
	}
	if err := query.Order("w.reset_at ASC, w.account_id ASC, w.mode ASC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]account.QuotaWindow, 0, len(rows))
	for _, row := range rows {
		window := toQuotaWindowDomain(row.Window)
		window.Provider = account.Provider(row.Provider)
		values = append(values, window)
	}
	return values, nil
}

// ListQuotaRecoveryWindows 返回恢复候选窗口的原始行：只做 remaining <= 0 这一
// 粗略超集过滤(domain owner 的 QuotaWindowDeservesRecovery 同样要求该条件)，恢复
// 资格由调用方按 owner 规则判定。这里有意不过滤 provider/mode 或 reset_at——
// Console 非用量计费快照与没有 reset 期限的用量窗口都在候选范围内。Provider 随行
// 返回以便调用方判定资格；缺失账号只会留下空 Provider，不会让候选行消失。
func (r *AccountRepository) ListQuotaRecoveryWindows(ctx context.Context, limit int) ([]account.QuotaWindow, error) {
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).Where("remaining <= 0").Order("reset_at ASC, account_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	providers, err := r.quotaWindowProviders(ctx, rows)
	if err != nil {
		return nil, err
	}
	values := make([]account.QuotaWindow, 0, len(rows))
	for _, row := range rows {
		value := toQuotaWindowDomain(row)
		value.Provider = account.Provider(providers[row.AccountID])
		values = append(values, value)
	}
	return values, nil
}

// quotaWindowProviders 按块读取候选窗口账号的 Provider：候选集合可能覆盖整个号池，
// 单条 IN 列表会超过 SQLite/PostgreSQL 的绑定参数上限。
func (r *AccountRepository) quotaWindowProviders(ctx context.Context, rows []quotaWindowModel) (map[uint64]string, error) {
	const providerBatchSize = 1000
	ids := make([]uint64, 0, len(rows))
	seen := make(map[uint64]bool, len(rows))
	for _, row := range rows {
		if seen[row.AccountID] {
			continue
		}
		seen[row.AccountID] = true
		ids = append(ids, row.AccountID)
	}
	providers := make(map[uint64]string, len(ids))
	for start := 0; start < len(ids); start += providerBatchSize {
		end := min(start+providerBatchSize, len(ids))
		var accounts []struct {
			ID       uint64
			Provider string
		}
		if err := r.db.db.WithContext(ctx).Table("provider_accounts").Select("id, provider").Where("id IN ?", ids[start:end]).Scan(&accounts).Error; err != nil {
			return nil, err
		}
		for _, value := range accounts {
			providers[value.ID] = value.Provider
		}
	}
	return providers, nil
}

// ListStaleWebQuotaAccountIDs 返回缺失或长期未同步额度的 Web 账号，供重启后的低优先级追赶任务使用。
func (r *AccountRepository) ListStaleWebQuotaAccountIDs(ctx context.Context, before time.Time, limit int) ([]uint64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Joins("LEFT JOIN account_quota_windows AS quota ON quota.account_id = account.id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderWeb, true, account.AuthStatusActive).
		Group("account.id").
		Having("MAX(quota.synced_at) IS NULL OR MAX(quota.synced_at) < ?", before.UTC()).
		Order("MIN(quota.synced_at) ASC, account.id ASC").
		Limit(limit).
		Scan(&ids).Error
	return ids, err
}

func toQuotaWindowDomain(row quotaWindowModel) account.QuotaWindow {
	var serializedBreakdown []quotaBreakdownJSON
	_ = json.Unmarshal([]byte(row.BreakdownJSON), &serializedBreakdown)
	result := toRoutingQuotaWindowDomain(row)
	breakdown := make([]account.QuotaBreakdown, 0, len(serializedBreakdown))
	for _, item := range serializedBreakdown {
		breakdown = append(breakdown, account.QuotaBreakdown{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
	}
	result.Breakdown = breakdown
	return result
}

func toRoutingQuotaWindowDomain(row quotaWindowModel) account.QuotaWindow {
	return account.QuotaWindow{
		AccountID: row.AccountID, Mode: row.Mode, SnapshotVersion: row.SnapshotVersion, Revision: row.Revision, Remaining: row.Remaining, Total: row.Total,
		UsagePercent: row.UsagePercent, WindowSeconds: row.WindowSeconds,
		ResetAt: row.ResetAt, SyncedAt: row.SyncedAt, Source: account.QuotaSource(row.Source), UpdatedAt: row.UpdatedAt,
	}
}
