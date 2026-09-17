package relational

// 账号事实读取存储实现(model.AccountFacts 消费角色的存储侧,
// 事务聚合):Get/List/ListEnabled/计数与账单读取——目录只读面。
// 行类型与建库在 account_repository.go;路由查询在
// account_routing_queries.go;生命周期写在各 account_*.go。

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"strconv"
	"strings"
)

func (r *AccountRepository) GetBillings(ctx context.Context, accountIDs []uint64) (map[uint64]account.Billing, error) {
	result := make(map[uint64]account.Billing, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []billingModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = toBillingDomain(row)
	}
	return result, nil
}

func (r *AccountRepository) GetBilling(ctx context.Context, accountID uint64) (account.Billing, error) {
	var row billingModel
	if err := r.db.db.WithContext(ctx).First(&row, "account_id = ?", accountID).Error; err != nil {
		return account.Billing{}, mapError(err)
	}
	return toBillingDomain(row), nil
}

func (r *AccountRepository) Get(ctx context.Context, id uint64) (account.Credential, error) {
	var row accountModel
	if err := r.db.db.WithContext(ctx).Preload("Credential").Preload("WebProfile").First(&row, id).Error; err != nil {
		return account.Credential{}, mapError(err)
	}
	value := toAccountDomain(row)
	values := []account.Credential{value}
	if err := r.attachAccountLinks(ctx, values); err != nil {
		return account.Credential{}, err
	}
	return values[0], nil
}

func (r *AccountRepository) ListEnabled(ctx context.Context, provider account.Provider) ([]account.Credential, error) {
	rows, err := r.listActiveProviderAccountRows(ctx, provider, nil)
	if err != nil {
		return nil, err
	}
	out := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAccountDomain(row))
	}
	if err := r.attachRoutingEgressIdentities(ctx, provider, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *AccountRepository) CountProviderAccountsByIDs(ctx context.Context, providerValue account.Provider, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var count int64
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("provider = ? AND id IN ?", providerValue, ids).
		Count(&count).Error
	return count, err
}

func (r *AccountRepository) List(ctx context.Context, input repository.AccountListQuery) ([]account.Credential, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&accountModel{})
	if input.Filter.Provider != "" {
		query = query.Where("provider = ?", input.Filter.Provider)
	}
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		if id, err := strconv.ParseUint(strings.TrimPrefix(search, "#"), 10, 64); strings.HasPrefix(search, "#") && err == nil && id > 0 {
			// #ID 是管理端名单使用的内部精确查询形式，走主键索引且不改变
			// 原有纯数字名称的模糊搜索语义。
			query = query.Where("provider_accounts.id = ?", id)
		} else {
			pattern := "%" + strings.ToLower(search) + "%"
			query = query.Where("LOWER(name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(user_id) LIKE ? OR LOWER(team_id) LIKE ?", pattern, pattern, pattern, pattern)
		}
	}
	switch input.Filter.QuotaType {
	case "free":
		// Super（Billing paid 或 BuildSuperEntitled）不得落入 free；与 IsKnownFreeBuild / QuotaView 一致。
		query = query.Where("NOT " + accountBuildSuperPredicate + " AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.kind = 'free') OR " + accountFreeSignalPredicate + ")")
	case "paid":
		query = query.Where(accountBuildSuperPredicate)
	case "unknown":
		query = query.Where("NOT " + accountRecoveryPredicate + " AND NOT " + accountBuildSuperPredicate + " AND NOT " + accountFreeSignalPredicate)
	case "auto", "basic", "super", "heavy":
		query = query.Where("EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id AND profile.tier = ?)", input.Filter.QuotaType)
	}
	query = applyAccountStatusFilter(query, input.Filter.Status, input.Filter.Now)
	if input.Filter.Refreshable != nil {
		if *input.Filter.Refreshable {
			query = query.Where("EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.encrypted_refresh <> '')")
		} else {
			query = query.Where("NOT EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.encrypted_refresh <> '')")
		}
	}
	switch input.Filter.Risk {
	case "flagged":
		query = query.Where("(EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.build_bot_flag_source IN (1,2)) OR provider_accounts.risk_status <> '')")
	case "normal":
		query = query.Where("NOT EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.build_bot_flag_source IN (1,2)) AND provider_accounts.risk_status = ''")
	}
	query = applyWebAgreementFilter(query, input.Filter.Agreement)
	query = applyAssociationFilter(query, input.Filter.Provider, input.Filter.Association)
	if input.Filter.RestrictIDs {
		if len(input.Filter.AccountIDs) == 0 {
			query = query.Where("1 = 0")
		} else {
			query = query.Where("provider_accounts.id IN ?", input.Filter.AccountIDs)
		}
	}
	if len(input.Filter.ExcludeIDs) > 0 {
		query = query.Where("provider_accounts.id NOT IN ?", input.Filter.ExcludeIDs)
	}
	if input.Filter.AfterID > 0 {
		query = query.Where("provider_accounts.id > ?", input.Filter.AfterID)
	}
	if input.Filter.ThroughID > 0 {
		query = query.Where("provider_accounts.id <= ?", input.Filter.ThroughID)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []accountModel
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"id":        {expression: "provider_accounts.id"},
		"name":      {expression: "LOWER(provider_accounts.name)"},
		"type":      {expression: accountTypeSortExpression},
		"status":    {expression: accountStatusSortExpression},
		"createdAt": {expression: "provider_accounts.created_at", defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "provider_accounts.created_at", defaultDirection: repository.SortDescending}, "provider_accounts.id")
	if err := query.Preload("Credential").Preload("WebProfile").Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
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
