package relational

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// Routing claim rows are scalar, secret-free projections. Joins avoid the
// round-trip and allocation cost of running each provider-wide loader for one
// selected account. Column lists are shared with the cached routing projection.
type routingClaimRow struct {
	Account         accountModel           `gorm:"embedded"`
	Material        accountCredentialModel `gorm:"embedded;embeddedPrefix:material_"`
	Profile         webAccountProfileModel `gorm:"embedded;embeddedPrefix:profile_"`
	Billing         billingModel           `gorm:"embedded;embeddedPrefix:billing_"`
	Recovery        quotaRecoveryModel     `gorm:"embedded;embeddedPrefix:recovery_"`
	LinkedSource    string
	LinkedIdentity  string
	CapabilityKnown bool
	SupportsModel   bool
	HasBindings     bool
	Bound           bool
}

func routingAliasedColumns(alias, prefix string, columns []string) string {
	values := make([]string, len(columns))
	for i, column := range columns {
		values[i] = alias + "." + column + " AS " + prefix + column
	}
	return strings.Join(values, ", ")
}

var routingClaimColumns = strings.Join([]string{
	"provider_accounts.*",
	routingAliasedColumns("material", "material_", routingCredentialMetadataColumns),
	routingAliasedColumns("profile", "profile_", []string{"account_id", "tier", "synced_at", "nsfw_enabled_at", "terms_accepted_at", "terms_accepted_version", "birth_date_set_at", "egress_identity"}),
	routingAliasedColumns("billing", "billing_", routingBillingColumns),
	routingAliasedColumns("recovery", "recovery_", []string{"account_id", "kind", "status", "confirmed_used", "confirmed_limit", "exhausted_at", "next_probe_at", "last_confirmed_at", "updated_at"}),
}, ", ")

// GetRoutingCandidate is a new physical claim's consistent view. It never uses
// a provider cache or a stale fallback. Missing/disabled/auth-inactive accounts
// and accounts outside current model bindings are absent; M06 owns all other
// eligibility decisions, including client-key scope and quality admission.
func (r *AccountRepository) GetRoutingCandidate(ctx context.Context, accountID uint64, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string) (account.RoutingCandidate, error) {
	if accountID == 0 {
		return account.RoutingCandidate{}, repository.ErrNotFound
	}
	var result account.RoutingCandidate
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		snapshot := NewAccountRepository(&Database{db: tx, dialect: r.db.dialect})
		var err error
		result, err = snapshot.readRoutingClaim(ctx, accountID, provider, modelRouteID, upstreamModel, quotaMode)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return account.RoutingCandidate{}, mapError(err)
	}
	return result, nil
}

func (r *AccountRepository) readRoutingClaim(ctx context.Context, id uint64, provider account.Provider, routeID uint64, model, mode string) (account.RoutingCandidate, error) {
	db := r.db.db.WithContext(ctx)
	now := time.Now().UTC()
	columns := routingClaimColumns
	var args []any
	if strings.TrimSpace(model) == "" {
		columns += ", FALSE AS capability_known, FALSE AS supports_model, FALSE AS has_bindings, FALSE AS bound"
	} else {
		bindings := func() *gorm.DB { return r.routingBindingsQuery(ctx, provider, routeID, model).Select("1") }
		columns += ", EXISTS (?) AS capability_known, EXISTS (?) AS supports_model, EXISTS (?) AS has_bindings, EXISTS (?) AS bound"
		args = append(args,
			db.Table("account_model_sync_states").Select("1").Where("account_id = ? AND last_success_at IS NOT NULL", id),
			db.Table("account_model_capabilities").Select("1").Where("account_id = ? AND upstream_model = ?", id, model),
			bindings(), bindings().Where("binding.account_id = ?", id))
	}
	query := db.Table("provider_accounts").
		Joins("LEFT JOIN account_credentials AS material ON material.account_id = provider_accounts.id").
		Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = provider_accounts.id").
		Joins("LEFT JOIN account_billing_snapshots AS billing ON billing.account_id = provider_accounts.id").
		Joins("LEFT JOIN account_quota_recovery AS recovery ON recovery.account_id = provider_accounts.id").
		Where("provider_accounts.id = ? AND provider_accounts.provider = ? AND provider_accounts.enabled = TRUE AND provider_accounts.auth_status = ?", id, provider, account.AuthStatusActive)
	if provider == account.ProviderBuild || provider == account.ProviderConsole {
		link := "account_provider_links"
		target := "build_account_id"
		if provider == account.ProviderConsole {
			link, target = "web_console_account_links", "console_account_id"
		}
		query = query.Joins("LEFT JOIN " + link + " AS link ON link." + target + " = provider_accounts.id").
			Joins("LEFT JOIN provider_accounts AS linked_web ON linked_web.id = link.web_account_id").
			Joins("LEFT JOIN web_account_profiles AS linked_profile ON linked_profile.account_id = linked_web.id")
		columns += ", linked_web.source_key AS linked_source, linked_profile.egress_identity AS linked_identity"
	}
	var row routingClaimRow
	if err := query.Select(columns, args...).Take(&row).Error; err != nil {
		return account.RoutingCandidate{}, err
	}
	if row.HasBindings && !row.Bound {
		return account.RoutingCandidate{}, repository.ErrNotFound
	}
	if row.Material.AccountID != 0 {
		row.Account.Credential = &row.Material
	}
	if row.Profile.AccountID != 0 {
		row.Account.WebProfile = &row.Profile
	}
	result := account.RoutingCandidate{Credential: toAccountDomain(row.Account)}
	if provider == account.ProviderBuild || provider == account.ProviderConsole {
		result.Credential.EgressIdentity = linkedWebEgressIdentity(row.LinkedIdentity, row.LinkedSource)
	}
	if row.Billing.AccountID != 0 {
		billing := toRoutingBillingDomain(row.Billing)
		result.Billing = &billing
	}
	if row.Recovery.AccountID != 0 {
		recovery := quotaRecoveryDomain(row.Recovery)
		result.QuotaRecovery = &recovery
	}
	sharedSuper := !row.HasBindings && row.SupportsModel && account.IsBuildSuper(result.Credential, result.Billing)
	if !row.HasBindings && !row.SupportsModel && provider == account.ProviderBuild && strings.TrimSpace(model) != "" && account.IsBuildSuper(result.Credential, result.Billing) {
		// Only an unbound Super account missing its own model capability needs
		// this global fact. Keep the large billing predicate out of ordinary
		// claim SQL; SQLite otherwise reparses it even when CASE skips execution.
		var ids []uint64
		if err := db.Model(&accountModel{}).Select("id").
			Where("provider = ? AND enabled = TRUE AND auth_status = ?", provider, account.AuthStatusActive).
			Where(accountBuildSuperPredicate).
			Where("EXISTS (SELECT 1 FROM account_model_capabilities capability WHERE capability.account_id = provider_accounts.id AND capability.upstream_model = ?)", model).
			Limit(1).Pluck("id", &ids).Error; err != nil {
			return account.RoutingCandidate{}, err
		}
		sharedSuper = len(ids) > 0
	}
	result.ModelCapabilityKnown, result.SupportsModel = account.RoutingModelCapability(provider, mode, row.HasBindings, sharedSuper, result.Credential, result.Billing, row.CapabilityKnown, row.SupportsModel)
	windows, err := r.getRoutingQuotaWindows(ctx, provider, mode, []account.Credential{result.Credential}, id)
	if err != nil {
		return account.RoutingCandidate{}, err
	}
	if window, ok := windows[id]; ok {
		result.QuotaWindow = &window
	}
	if strings.TrimSpace(model) != "" {
		var blocks []accountModelQuotaBlockModel
		if err := db.Where("account_id = ? AND upstream_model = ? AND cooldown_until > ?", id, model, now).Find(&blocks).Error; err != nil {
			return account.RoutingCandidate{}, err
		}
		for _, row := range blocks {
			block := modelRestrictionDomain(row)
			if result.ModelQuotaBlock != nil {
				block = account.DominantModelRestriction(*result.ModelQuotaBlock, block)
			}
			result.ModelQuotaBlock = &block
		}
	}
	return result, nil
}

func routingAccountFilter(column string, accountID uint64) func(*gorm.DB) *gorm.DB {
	return func(query *gorm.DB) *gorm.DB {
		if accountID != 0 {
			return query.Where(column+" = ?", accountID)
		}
		return query
	}
}
