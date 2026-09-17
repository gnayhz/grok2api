package relational

// 账号路由查询存储实现(selector.RoutingStore 消费角色的存储侧,
// 事务聚合):路由候选、基础账号与路由账单读取及行查询助手。
// 行类型与建库在 account_repository.go;资格领取在 account_routing_claim.go。

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"strings"
	"time"
)

func (r *AccountRepository) getRoutingBillings(ctx context.Context, provider account.Provider) (map[uint64]account.Billing, error) {
	result := make(map[uint64]account.Billing)
	var rows []billingModel
	if err := r.db.db.WithContext(ctx).
		Table("account_billing_snapshots AS billing").
		Select(qualifiedColumnList("billing", routingBillingColumns)).
		Joins("JOIN provider_accounts AS account ON account.id = billing.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = toRoutingBillingDomain(row)
	}
	return result, nil
}

func qualifiedColumnList(alias string, columns []string) string {
	qualified := make([]string, 0, len(columns))
	for _, column := range columns {
		qualified = append(qualified, alias+"."+column)
	}
	return strings.Join(qualified, ", ")
}

func (r *AccountRepository) listActiveProviderAccountRows(ctx context.Context, provider account.Provider, credentialColumns []string) ([]accountModel, error) {
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).
		Where("provider = ? AND enabled = ? AND auth_status = ?", provider, true, account.AuthStatusActive).
		Order("priority DESC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return rows, nil
	}
	positions := make(map[uint64]int, len(rows))
	for index := range rows {
		positions[rows[index].ID] = index
	}

	credentialSelect := "credential.*"
	if len(credentialColumns) > 0 {
		credentialSelect = qualifiedColumnList("credential", credentialColumns)
	}
	var credentials []accountCredentialModel
	if err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select(credentialSelect).
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive).
		Find(&credentials).Error; err != nil {
		return nil, err
	}
	for index := range credentials {
		if position, ok := positions[credentials[index].AccountID]; ok {
			rows[position].Credential = &credentials[index]
		}
	}

	if provider == account.ProviderWeb {
		var profiles []webAccountProfileModel
		if err := r.db.db.WithContext(ctx).
			Table("web_account_profiles AS profile").
			Select("profile.*").
			Joins("JOIN provider_accounts AS account ON account.id = profile.account_id").
			Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive).
			Find(&profiles).Error; err != nil {
			return nil, err
		}
		for index := range profiles {
			if position, ok := positions[profiles[index].AccountID]; ok {
				rows[position].WebProfile = &profiles[index]
			}
		}
	}
	return rows, nil
}

func (r *AccountRepository) listRoutingCredentials(ctx context.Context, provider account.Provider) ([]account.Credential, error) {
	rows, err := r.listActiveProviderAccountRows(ctx, provider, routingCredentialMetadataColumns)
	if err != nil {
		return nil, err
	}
	values := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		values = append(values, toAccountDomain(row))
	}
	if err := r.attachRoutingEgressIdentities(ctx, provider, values); err != nil {
		return nil, err
	}
	return values, nil
}

func (r *AccountRepository) ListRoutingAccountBases(ctx context.Context, provider account.Provider, quotaMode string) ([]account.RoutingAccountBase, error) {
	values, err := r.listRoutingCredentials(ctx, provider)
	if err != nil {
		return nil, mapError(err)
	}
	billings, err := r.getRoutingBillings(ctx, provider)
	if err != nil {
		return nil, mapError(err)
	}
	recoveries, err := r.getRoutingQuotaRecoveries(ctx, provider)
	if err != nil {
		return nil, mapError(err)
	}
	quotaWindows, err := r.getRoutingQuotaWindows(ctx, provider, quotaMode, values, 0)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]account.RoutingAccountBase, 0, len(values))
	for _, value := range values {
		base := account.RoutingAccountBase{Credential: value}
		if billing, ok := billings[value.ID]; ok {
			base.Billing = &billing
		}
		if recovery, ok := recoveries[value.ID]; ok {
			base.QuotaRecovery = &recovery
		}
		if window, ok := quotaWindows[value.ID]; ok {
			base.QuotaWindow = &window
		}
		result = append(result, base)
	}
	return result, nil
}

func (r *AccountRepository) ListRoutingCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string) ([]account.RoutingCandidate, error) {
	values, err := r.listRoutingCredentials(ctx, provider)
	if err != nil {
		return nil, mapError(err)
	}
	bound := make(map[uint64]bool)
	if strings.TrimSpace(upstreamModel) != "" {
		boundIDs, loadErr := r.listRoutingBoundAccountIDs(ctx, provider, modelRouteID, upstreamModel)
		if loadErr != nil {
			return nil, mapError(loadErr)
		}
		if len(boundIDs) > 0 {
			for _, id := range boundIDs {
				bound[id] = true
			}
			filtered := values[:0]
			for _, value := range values {
				if bound[value.ID] {
					filtered = append(filtered, value)
				}
			}
			values = filtered
		}
	}
	billings, err := r.getRoutingBillings(ctx, provider)
	if err != nil {
		return nil, mapError(err)
	}
	recoveries, err := r.getRoutingQuotaRecoveries(ctx, provider)
	if err != nil {
		return nil, mapError(err)
	}
	quotaWindows, err := r.getRoutingQuotaWindows(ctx, provider, quotaMode, values, 0)
	if err != nil {
		return nil, mapError(err)
	}
	known := make(map[uint64]bool, len(values))
	supported := make(map[uint64]bool, len(values))
	modelQuotaBlocks := make(map[uint64]account.ModelQuotaBlock, len(values))
	if strings.TrimSpace(upstreamModel) != "" && len(values) > 0 {
		var states []accountModelSyncStateModel
		if err := r.db.db.WithContext(ctx).
			Table("account_model_sync_states AS state").
			Select("state.*").
			Joins("JOIN provider_accounts AS account ON account.id = state.account_id").
			Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND state.last_success_at IS NOT NULL", provider, true, account.AuthStatusActive).
			Find(&states).Error; err != nil {
			return nil, mapError(err)
		}
		for _, state := range states {
			known[state.AccountID] = true
		}
		var capabilities []accountModelCapabilityModel
		if err := r.db.db.WithContext(ctx).
			Table("account_model_capabilities AS capability").
			Select("capability.*").
			Joins("JOIN provider_accounts AS account ON account.id = capability.account_id").
			Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND capability.upstream_model = ?", provider, true, account.AuthStatusActive, upstreamModel).
			Find(&capabilities).Error; err != nil {
			return nil, mapError(err)
		}
		for _, capability := range capabilities {
			supported[capability.AccountID] = true
		}
		var blockRows []accountModelQuotaBlockModel
		if err := r.db.db.WithContext(ctx).
			Table("account_model_quota_blocks AS block").
			Select("block.*").
			Joins("JOIN provider_accounts AS account ON account.id = block.account_id").
			Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND block.upstream_model = ? AND block.cooldown_until > ?", provider, true, account.AuthStatusActive, upstreamModel, time.Now().UTC()).
			Find(&blockRows).Error; err != nil {
			return nil, mapError(err)
		}
		for _, row := range blockRows {
			modelQuotaBlocks[row.AccountID] = account.DominantModelRestriction(modelQuotaBlocks[row.AccountID], modelRestrictionDomain(row))
		}
	}
	sharedSuperBuildModel := false
	if provider == account.ProviderBuild && len(bound) == 0 {
		for _, value := range values {
			if !supported[value.ID] {
				continue
			}
			var billing *account.Billing
			if snapshot, exists := billings[value.ID]; exists {
				billing = &snapshot
			}
			if account.IsBuildSuper(value, billing) {
				sharedSuperBuildModel = true
				break
			}
		}
	}
	result := make([]account.RoutingCandidate, 0, len(values))
	for _, value := range values {
		var billing *account.Billing
		if snapshot, exists := billings[value.ID]; exists {
			billing = &snapshot
		}
		capabilityKnown, supportsModel := account.RoutingModelCapability(provider, quotaMode, len(bound) > 0, sharedSuperBuildModel, value, billing, known[value.ID], supported[value.ID])
		candidate := account.RoutingCandidate{Credential: value, ModelCapabilityKnown: capabilityKnown, SupportsModel: supportsModel}
		if billing, ok := billings[value.ID]; ok {
			candidate.Billing = &billing
		}
		if recovery, ok := recoveries[value.ID]; ok {
			candidate.QuotaRecovery = &recovery
		}
		if window, ok := quotaWindows[value.ID]; ok {
			candidate.QuotaWindow = &window
		}
		if block, ok := modelQuotaBlocks[value.ID]; ok {
			candidate.ModelQuotaBlock = &block
		}
		result = append(result, candidate)
	}
	return result, nil
}
