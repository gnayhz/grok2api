package relational

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ModelRepository struct {
	db       *Database
	observer repository.InvalidationObserver
}

// Console static support is anchored to the reconciled catalog routes instead
// of the provider name alone. Manual aliases remain supported while an
// equivalent catalog route exists, and stale aliases stop advertising support
// as soon as that catalog entry is removed.
const modelConsoleStaticSupportExpression = `(model_routes.provider = 'grok_console' AND (
	model_routes.origin = 'catalog'
	OR EXISTS (
		SELECT 1 FROM model_routes console_catalog_route
		WHERE console_catalog_route.provider = model_routes.provider
			AND console_catalog_route.upstream_model = model_routes.upstream_model
			AND console_catalog_route.capability = model_routes.capability
			AND console_catalog_route.origin = 'catalog'
	)
))`

// These Web media catalog capabilities are available to Basic accounts for
// their confirmed quota products. Older capability snapshots may predate that
// support, so catalog routes (and their aliases) remain discoverable while the
// runtime selector enforces the adapter's parameter-specific tier order.
const modelWebBasicMediaStaticSupportExpression = `(model_routes.provider = 'grok_web'
	AND model_routes.upstream_model IN ('grok-imagine-image-quality', 'grok-imagine-image-2.0', 'imagine-image-edit', 'grok-imagine-video')
	AND (
		model_routes.origin = 'catalog'
		OR EXISTS (
			SELECT 1 FROM model_routes web_catalog_route
			WHERE web_catalog_route.provider = model_routes.provider
				AND web_catalog_route.upstream_model = model_routes.upstream_model
				AND web_catalog_route.capability = model_routes.capability
				AND web_catalog_route.origin = 'catalog'
		)
	))`

const availableRoutePredicate = `
	EXISTS (
		SELECT 1 FROM provider_accounts account
		WHERE account.provider = model_routes.provider
			AND account.enabled = ?
			AND account.auth_status = ?
			AND (
				EXISTS (
					SELECT 1 FROM model_route_accounts binding
					WHERE binding.model_route_id = model_routes.id
						AND binding.account_id = account.id
				)
				OR (
					NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id)
					AND (
						` + modelConsoleStaticSupportExpression + `
						OR ` + modelWebBasicMediaStaticSupportExpression + `
						OR EXISTS (
							SELECT 1 FROM account_model_capabilities capability
							WHERE capability.account_id = account.id
								AND capability.upstream_model = model_routes.upstream_model
						)
					)
				)
			)
	)
`

// Build Super 共享能力：Billing paid 或 build_super_entitled；仅 grok_build。
const modelAccountBuildSuperPredicate = `(EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = account.id AND ` + accountPaidBillingSignals + `) OR (account.provider = 'grok_build' AND account.build_super_entitled = TRUE))`
const modelPeerBuildSuperPredicate = `(EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = peer.id AND ` + accountPaidBillingSignals + `) OR (peer.provider = 'grok_build' AND peer.build_super_entitled = TRUE))`

const modelSharedPaidBuildSupportSortExpression = `(model_routes.provider = 'grok_build'
	AND ` + modelAccountBuildSuperPredicate + `
	AND EXISTS (
		SELECT 1
		FROM provider_accounts peer
		JOIN account_model_capabilities peer_capability ON peer_capability.account_id = peer.id AND peer_capability.upstream_model = model_routes.upstream_model
		WHERE peer.provider = model_routes.provider
			AND peer.enabled = TRUE
			AND peer.auth_status = 'active'
			AND ` + modelPeerBuildSuperPredicate + `
	))`

// These predicates mirror the gateway's client-key scope classification. They
// are used only by the admin model picker to avoid presenting routes that the
// selected account scope can never serve.
const modelAccountBuildFreePredicate = `(account.provider = 'grok_build'
	AND NOT ` + modelAccountBuildSuperPredicate + `
	AND (
		EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = account.id AND recovery.kind = 'free')
		OR LOWER(TRIM(account.observed_model)) LIKE '%-build-free'
		OR EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = account.id AND ` + accountFreeBillingSignal + `)
	))`

const modelAccountBuildSuperTierPredicate = `(account.provider = 'grok_build' AND ` + modelAccountBuildSuperPredicate + `)`

const modelAccountWebFreePredicate = `(account.provider = 'grok_web'
	AND EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = account.id AND profile.tier = 'basic'))`

const modelAccountWebSuperPredicate = `(account.provider = 'grok_web'
	AND EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = account.id AND profile.tier IN ('super', 'heavy')))`

const modelSharedPaidBuildScopeExpression = `(model_routes.provider = 'grok_build'
	AND ` + modelAccountBuildSuperPredicate + `
	AND EXISTS (
		SELECT 1
		FROM provider_accounts peer
		JOIN account_model_capabilities peer_capability ON peer_capability.account_id = peer.id AND peer_capability.upstream_model = model_routes.upstream_model
		WHERE peer.provider = model_routes.provider
			AND ` + modelPeerBuildSuperPredicate + `
	))`

const modelRouteAccountCapabilityPredicate = `(
	EXISTS (
		SELECT 1 FROM model_route_accounts binding
		WHERE binding.model_route_id = model_routes.id
			AND binding.account_id = account.id
	)
	OR (
		NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id)
		AND (
			` + modelConsoleStaticSupportExpression + `
			OR ` + modelWebBasicMediaStaticSupportExpression + `
			OR EXISTS (
				SELECT 1 FROM account_model_capabilities capability
				WHERE capability.account_id = account.id
					AND capability.upstream_model = model_routes.upstream_model
			)
			OR ` + modelSharedPaidBuildScopeExpression + `
		)
	)
)`

const modelAvailableRouteAccountCapabilityPredicate = `(
	EXISTS (
		SELECT 1 FROM model_route_accounts binding
		WHERE binding.model_route_id = model_routes.id
			AND binding.account_id = account.id
	)
	OR (
		NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id)
		AND (
			` + modelConsoleStaticSupportExpression + `
			OR ` + modelWebBasicMediaStaticSupportExpression + `
			OR EXISTS (
				SELECT 1 FROM account_model_capabilities capability
				WHERE capability.account_id = account.id
					AND capability.upstream_model = model_routes.upstream_model
			)
			OR ` + modelSharedPaidBuildSupportSortExpression + `
		)
	)
)`

func modelTierAvailabilityPredicate(tiers []string) string {
	return modelTierAvailabilityPredicateWithAvailability(tiers, false)
}

func modelTierAvailabilityPredicateWithAvailability(tiers []string, activeOnly bool) string {
	parts := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		switch tier {
		case "free":
			parts = append(parts, modelAccountBuildFreePredicate, modelAccountWebFreePredicate)
		case "super":
			parts = append(parts, modelAccountBuildSuperTierPredicate, modelAccountWebSuperPredicate)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	accountPredicate := ""
	capabilityPredicate := modelRouteAccountCapabilityPredicate
	if activeOnly {
		accountPredicate = " AND account.enabled = TRUE AND account.auth_status = 'active'"
		capabilityPredicate = modelAvailableRouteAccountCapabilityPredicate
	}
	return `(model_routes.provider = 'grok_console' OR EXISTS (
		SELECT 1 FROM provider_accounts account
		WHERE account.provider = model_routes.provider
		` + accountPredicate + `
			AND (` + strings.Join(parts, " OR ") + `)
			AND ` + capabilityPredicate + `
	))`
}

const (
	modelProviderPriorityExpression = "CASE model_routes.provider WHEN 'grok_build' THEN 0 WHEN 'grok_web' THEN 1 WHEN 'grok_console' THEN 2 ELSE 3 END"
	modelSupportSortExpression      = `(SELECT COUNT(*) FROM provider_accounts account WHERE account.provider = model_routes.provider AND account.enabled = TRUE AND account.auth_status = 'active' AND (EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id AND binding.account_id = account.id) OR (NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id) AND (` + modelConsoleStaticSupportExpression + ` OR ` + modelWebBasicMediaStaticSupportExpression + ` OR EXISTS (SELECT 1 FROM account_model_capabilities capability WHERE capability.account_id = account.id AND capability.upstream_model = model_routes.upstream_model) OR ` + modelSharedPaidBuildSupportSortExpression + `))))`
	modelSyncedSortExpression       = `(SELECT MAX(sync.last_success_at) FROM provider_accounts account JOIN account_model_sync_states sync ON sync.account_id = account.id WHERE account.provider = model_routes.provider AND account.enabled = TRUE AND account.auth_status = 'active')`
)

func NewModelRepository(db *Database) *ModelRepository { return &ModelRepository{db: db} }

func (r *ModelRepository) SetInvalidationObserver(observer repository.InvalidationObserver) {
	r.observer = observer
}

func (r *ModelRepository) notifyInvalidation(ctx context.Context, event repository.InvalidationEvent) {
	if r.observer != nil {
		r.observer(ctx, event)
	}
}

func (r *ModelRepository) List(ctx context.Context, input repository.ModelListQuery) ([]model.Route, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&modelRouteModel{})
	if input.Filter.ActiveScope {
		query = r.availableRoutes(query)
	}
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(public_id) LIKE ? OR LOWER(upstream_model) LIKE ?", pattern, pattern)
	}
	if input.Filter.Provider != "" {
		query = query.Where("provider = ?", input.Filter.Provider)
	}
	if len(input.Filter.Providers) > 0 {
		query = query.Where("provider IN ?", input.Filter.Providers)
	}
	tierPredicate := modelTierAvailabilityPredicate(input.Filter.Tiers)
	if input.Filter.ActiveScope {
		tierPredicate = modelTierAvailabilityPredicateWithAvailability(input.Filter.Tiers, true)
	}
	if tierPredicate != "" {
		query = query.Where(tierPredicate)
	}
	if input.Filter.Enabled != nil {
		query = query.Where("enabled = ?", *input.Filter.Enabled)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []modelRouteModel
	if input.Page.Sort.Field == "accountSupport" {
		predicate, args := modelCapabilityPredicate()
		query = query.Select("model_routes.*, CASE WHEN "+predicate+" THEN "+modelSupportSortExpression+" ELSE 0 END AS support_sort", args...)
	}
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"publicId":       {expression: "LOWER(model_routes.public_id)"},
		"upstreamModel":  {expression: "LOWER(model_routes.upstream_model)"},
		"status":         {expression: "CASE WHEN model_routes.enabled = TRUE THEN 0 ELSE 1 END"},
		"provider":       {expression: "model_routes.provider"},
		"accountSupport": {expression: "support_sort", defaultDirection: repository.SortDescending},
		"lastSyncedAt":   {expression: modelSyncedSortExpression, nullsLast: true, defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "model_routes.created_at", defaultDirection: repository.SortDescending}, "model_routes.id")
	if err := query.Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, 0, err
	}
	return values, total, nil
}

type modelRouteGroupRow struct {
	Provider      string
	PublicID      string
	UpstreamModel string
	Origin        string
	ManualRouteID uint64
	RouteIDs      string
}

// ListGroups paginates logical model targets before loading their member
// routes. This keeps capability groups complete under filters and avoids the
// admin UI fetching and annotating the entire route table.
func (r *ModelRepository) ListGroups(ctx context.Context, input repository.ModelListQuery) ([]model.RouteGroup, int64, error) {
	// Route-level metrics must be calculated before grouping. In particular,
	// PostgreSQL does not allow a correlated metric to reference model_routes.id
	// from inside a grouped SELECT when id is not itself a grouping key.
	buildRouteMetricsQuery := func(includeMetrics bool) *gorm.DB {
		var selectArgs []any
		columns := []string{
			"model_routes.id",
			"model_routes.provider",
			"model_routes.public_id",
			"model_routes.upstream_model",
			"model_routes.origin",
			"model_routes.enabled",
			"model_routes.created_at",
		}
		if includeMetrics && input.Page.Sort.Field == "accountSupport" {
			predicate, args := modelCapabilityPredicate()
			columns = append(columns, "CASE WHEN "+predicate+" THEN "+modelSupportSortExpression+" ELSE 0 END AS support_sort")
			selectArgs = args
		}
		if includeMetrics && input.Page.Sort.Field == "lastSyncedAt" {
			columns = append(columns, modelSyncedSortExpression+" AS last_synced_sort")
		}
		query := r.db.db.WithContext(ctx).Model(&modelRouteModel{}).Select(strings.Join(columns, ", "), selectArgs...)
		if search := strings.TrimSpace(input.Page.Search); search != "" {
			pattern := "%" + strings.ToLower(search) + "%"
			query = query.Where("LOWER(model_routes.public_id) LIKE ? OR LOWER(model_routes.upstream_model) LIKE ?", pattern, pattern)
		}
		if input.Filter.Provider != "" {
			query = query.Where("model_routes.provider = ?", input.Filter.Provider)
		}
		return query
	}

	manualRouteIDExpression := "CASE WHEN route_metrics.origin = 'manual' THEN route_metrics.id ELSE 0 END"
	routeIDsExpression := "GROUP_CONCAT(CAST(route_metrics.id AS TEXT), ',')"
	if r.db.dialect == "postgres" {
		routeIDsExpression = "STRING_AGG(CAST(route_metrics.id AS TEXT), ',' ORDER BY route_metrics.id)"
	}
	enabledCountExpression := "SUM(CASE WHEN route_metrics.enabled = TRUE THEN 1 ELSE 0 END)"
	groupColumns := "route_metrics.provider, route_metrics.public_id, route_metrics.upstream_model, route_metrics.origin, " + manualRouteIDExpression
	supportSortExpression := "0"
	if input.Page.Sort.Field == "accountSupport" {
		supportSortExpression = "MAX(route_metrics.support_sort)"
	}
	lastSyncedSortExpression := "NULL"
	if input.Page.Sort.Field == "lastSyncedAt" {
		lastSyncedSortExpression = "MAX(route_metrics.last_synced_sort)"
	}

	buildQuery := func(includeMetrics bool) *gorm.DB {
		query := r.db.db.WithContext(ctx).Table("(?) AS route_metrics", buildRouteMetricsQuery(includeMetrics))
		if includeMetrics {
			query = query.Select(fmt.Sprintf(`
				route_metrics.provider,
				route_metrics.public_id,
				route_metrics.upstream_model,
				route_metrics.origin,
				%s AS manual_route_id,
				%s AS route_ids,
				CASE
					WHEN %s = COUNT(*) THEN 0
					WHEN %s > 0 THEN 1
					ELSE 2
				END AS status_sort,
				%s AS group_support_sort,
				%s AS group_last_synced_sort,
				MAX(route_metrics.created_at) AS created_sort
			`, manualRouteIDExpression, routeIDsExpression, enabledCountExpression, enabledCountExpression, supportSortExpression, lastSyncedSortExpression))
		} else {
			query = query.Select("route_metrics.provider, route_metrics.public_id, route_metrics.upstream_model, route_metrics.origin, " + manualRouteIDExpression + " AS manual_route_id")
		}
		query = query.Group(groupColumns)
		if input.Filter.Enabled != nil {
			if *input.Filter.Enabled {
				query = query.Having(enabledCountExpression + " > 0")
			} else {
				query = query.Having(enabledCountExpression + " = 0")
			}
		}
		return query
	}

	var total int64
	if err := r.db.db.WithContext(ctx).Table("(?) AS grouped_model_routes", buildQuery(false)).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	query := buildQuery(true)
	spec, direction := stableSortSpec(input.Page.Sort, map[string]sortSpec{
		"publicId":       {expression: "LOWER(route_metrics.public_id)"},
		"upstreamModel":  {expression: "LOWER(route_metrics.upstream_model)"},
		"status":         {expression: "status_sort"},
		"provider":       {expression: "route_metrics.provider"},
		"accountSupport": {expression: "group_support_sort", defaultDirection: repository.SortDescending},
		"lastSyncedAt":   {expression: "group_last_synced_sort", nullsLast: true, defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "created_sort", defaultDirection: repository.SortDescending})
	if spec.nullsLast {
		// PostgreSQL permits a SELECT alias as a plain ORDER BY item, but not
		// inside another ORDER BY expression. Repeat the aggregate only for the
		// NULL discriminator and use the alias for the value ordering below.
		nullSortExpression := spec.expression
		if input.Page.Sort.Field == "lastSyncedAt" {
			nullSortExpression = lastSyncedSortExpression
		}
		query = query.Order("CASE WHEN " + nullSortExpression + " IS NULL THEN 1 ELSE 0 END ASC")
	}
	query = query.Order(spec.expression + " " + direction).
		Order("route_metrics.provider ASC").
		Order("LOWER(route_metrics.public_id) ASC").
		Order("LOWER(route_metrics.upstream_model) ASC").
		Order("route_metrics.origin ASC").
		Order("manual_route_id ASC")
	var groupRows []modelRouteGroupRow
	if err := query.Offset(input.Page.Offset).Limit(input.Page.Limit).Scan(&groupRows).Error; err != nil {
		return nil, 0, err
	}

	groupIDs := make([][]uint64, 0, len(groupRows))
	allIDs := make([]uint64, 0, len(groupRows)*2)
	for _, row := range groupRows {
		ids, err := parseModelRouteGroupIDs(row.RouteIDs)
		if err != nil {
			return nil, 0, err
		}
		groupIDs = append(groupIDs, ids)
		allIDs = append(allIDs, ids...)
	}
	if len(allIDs) == 0 {
		return []model.RouteGroup{}, total, nil
	}
	var rows []modelRouteModel
	if err := r.db.db.WithContext(ctx).Where("id IN ?", allIDs).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, 0, err
	}
	byID := make(map[uint64]model.Route, len(values))
	for _, value := range values {
		byID[value.ID] = value
	}
	groups := make([]model.RouteGroup, 0, len(groupIDs))
	for _, ids := range groupIDs {
		members := make([]model.Route, 0, len(ids))
		for _, id := range ids {
			if value, ok := byID[id]; ok {
				members = append(members, value)
			}
		}
		if len(members) == 0 {
			continue
		}
		groups = append(groups, model.RouteGroup{Routes: members})
	}
	return groups, total, nil
}

func parseModelRouteGroupIDs(value string) ([]uint64, error) {
	parts := strings.Split(strings.TrimSpace(value), ",")
	ids := make([]uint64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("模型能力组包含无效路由 ID %q", part)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids, nil
}

func (r *ModelRepository) ListEnabled(ctx context.Context) ([]model.Route, error) {
	var rows []modelRouteModel
	if err := r.availableRoutes(r.db.db.WithContext(ctx)).Where("enabled = ?", true).Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

func (r *ModelRepository) ListEnabledForScope(ctx context.Context, filter repository.ModelListFilter) ([]model.Route, error) {
	query := r.availableRoutes(r.db.db.WithContext(ctx)).Where("enabled = ?", true)
	if len(filter.Providers) > 0 {
		query = query.Where("provider IN ?", filter.Providers)
	}
	if tierPredicate := modelTierAvailabilityPredicateWithAvailability(filter.Tiers, true); tierPredicate != "" {
		query = query.Where(tierPredicate)
	}
	var rows []modelRouteModel
	if err := query.Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

// ListConfiguredEnabled 返回所有已启用配置，包括暂时没有可用账号的路由，供 readiness 展示部分故障。
func (r *ModelRepository) ListConfiguredEnabled(ctx context.Context) ([]model.Route, error) {
	var rows []modelRouteModel
	if err := r.db.db.WithContext(ctx).Where("enabled = ?", true).Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

func (r *ModelRepository) Get(ctx context.Context, id uint64) (model.Route, error) {
	var row modelRouteModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return model.Route{}, mapError(err)
	}
	values := []model.Route{toModelDomain(row)}
	if err := r.annotateAvailability(ctx, values); err != nil {
		return model.Route{}, err
	}
	return values[0], nil
}

func (r *ModelRepository) GetByPublicID(ctx context.Context, publicID string) (model.Route, error) {
	values, err := r.GetByPublicIDCandidates(ctx, publicID)
	if err != nil {
		return model.Route{}, mapError(err)
	}
	return values[0], nil
}

// GetByPublicIDCandidates 返回同一下游模型名称当前可用的全部来源路由。
// 返回顺序遵循 Build、Web、Console 的稳定 Provider 顺序。
func (r *ModelRepository) GetByPublicIDCandidates(ctx context.Context, publicID string) ([]model.Route, error) {
	// Account availability is a fact on the matched name, never a predicate
	// that can erase a higher-priority name and reinterpret the request.
	db := r.db.db.WithContext(ctx).Model(&modelRouteModel{}).
		Select("model_routes.*, "+availableRoutePredicate+" AS route_available", true, account.AuthStatusActive)
	rows, err := findModelRoutesByPublicID(db, publicID)
	if err != nil {
		return nil, mapError(err)
	}
	facts := make([]model.PublicRoute, 0, len(rows))
	for _, row := range rows {
		facts = append(facts, model.PublicRoute{Route: toModelDomain(row.Route), AccountAvailable: row.RouteAvailable})
	}
	candidates := model.ClassifyCandidates(facts)
	if len(candidates.Routes) == 0 {
		return nil, &repository.ModelRouteUnavailableError{Enabled: candidates.Enabled, Unsupported: candidates.Unsupported}
	}
	return candidates.Routes, nil
}

// HasEnabledRouteByPublicID 报告该公开名是否存在已启用路由——刻意不带
// availableRoutePredicate（启用账号存在性）：调用方用它区分「模型不存在」
// （404）与「路由在但当前无可用账号」（503 upstream_unavailable）。
func (r *ModelRepository) HasEnabledRouteByPublicID(ctx context.Context, publicID string) (bool, error) {
	db := r.db.db.WithContext(ctx).Model(&modelRouteModel{})
	rows, err := findModelRoutesByPublicID(db, publicID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, mapError(err)
	}
	for _, row := range rows {
		if row.Route.Enabled {
			return true, nil
		}
	}
	return false, nil
}

func (r *ModelRepository) GetByPublicIDIncludingDisabled(ctx context.Context, publicID string) (model.Route, error) {
	db := r.db.db.WithContext(ctx)
	rows, err := findModelRoutesByPublicID(db, publicID)
	if err != nil {
		return model.Route{}, mapError(err)
	}
	return toModelDomain(rows[0].Route), nil
}

// modelRouteLookupRow is a read projection, not a schema column. Queries that
// need routing availability select it beside the unchanged persisted identity.
type modelRouteLookupRow struct {
	Route          modelRouteModel `gorm:"embedded"`
	RouteAvailable bool            `gorm:"column:route_available"`
}

// Callers must not filter enabled/account state before resolving name priority.
func findModelRoutesByPublicID(db *gorm.DB, publicID string) ([]modelRouteLookupRow, error) {
	db = db.Session(&gorm.Session{}).Model(&modelRouteModel{})
	if _, projected := db.Statement.Clauses["SELECT"]; !projected && len(db.Statement.Selects) == 0 {
		db = db.Select("model_routes.*")
	}
	for _, group := range model.NameLookupGroups(publicID) {
		rows, err := findModelRoutesByPublicIDGroup(db, group)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return rows, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

// findModelRoutesByPublicIDGroup 在单个公开 ID 候选组内解析路由。
// round 33 优化：原单条 "public_id IN ... OR EXISTS(alias)" 查询在路由表
// 增大后退化为 enabled 全扫 + 临时 B 树全排序（1000 路由实测 6ms，EXPLAIN
// 证实 OR 阻止复合索引点查）。改写为两个索引点查分支 + 应用层合并排序：
// public_id 分支走复合索引，别名分支走 alias 索引 + rowid 回表——均为
// O(log n)。合并行数为两候选集命中总和（实践中个位数），应用层排序成本
// 可忽略。排序键与原 ORDER BY 逐项等价：候选名命中优先于别名命中、
// Provider 优先级、id 升序。账号可用性仅可投影在匹配行上，不可预先过滤
// 配置行；否则上层会把不可用的字面名称误解为不存在而切换名称组。
func findModelRoutesByPublicIDGroup(db *gorm.DB, group model.NameLookupGroup) ([]modelRouteLookupRow, error) {
	candidates, aliasCandidates := group.PrimaryIDs, group.AliasIDs
	byID := make(map[uint64]modelRouteLookupRow, len(candidates)+len(aliasCandidates))
	direct := make(map[uint64]bool, len(candidates))
	if len(candidates) > 0 {
		var directRows []modelRouteLookupRow
		if err := db.Session(&gorm.Session{}).Where("public_id IN ?", candidates).Find(&directRows).Error; err != nil {
			return nil, err
		}
		for _, row := range directRows {
			byID[row.Route.ID] = row
			direct[row.Route.ID] = true
		}
	}
	var aliasRows []modelRouteLookupRow
	if err := db.Session(&gorm.Session{}).
		Where("id IN (SELECT model_route_id FROM model_route_aliases WHERE alias IN ? AND replaced_by_catalog = ?)", aliasCandidates, false).
		Find(&aliasRows).Error; err != nil {
		return nil, err
	}
	for _, row := range aliasRows {
		if _, exists := byID[row.Route.ID]; !exists {
			byID[row.Route.ID] = row
		}
	}
	rows := make([]modelRouteLookupRow, 0, len(byID))
	for _, row := range byID {
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		leftDirect, rightDirect := direct[left.Route.ID], direct[right.Route.ID]
		if leftDirect != rightDirect {
			return leftDirect
		}
		leftPriority, rightPriority := routeProviderPriority(left.Route.Provider), routeProviderPriority(right.Route.Provider)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return left.Route.ID < right.Route.ID
	})
	return rows, nil
}

// routeProviderPriority 与 SQL 的 modelProviderPriorityExpression 语义一致
// （grok_build < grok_web < grok_console < 其它）。
func routeProviderPriority(provider string) int {
	switch provider {
	case "grok_build":
		return 0
	case "grok_web":
		return 1
	case "grok_console":
		return 2
	default:
		return 3
	}
}

func (r *ModelRepository) GetByProviderUpstream(ctx context.Context, provider account.Provider, upstreamModel string) (model.Route, error) {
	var rows []modelRouteModel
	if err := r.availableRoutes(r.db.db.WithContext(ctx)).Where("provider = ? AND upstream_model = ? AND enabled = ?", provider, upstreamModel, true).Find(&rows).Error; err != nil {
		return model.Route{}, mapError(err)
	}
	if len(rows) == 0 {
		return model.Route{}, repository.ErrNotFound
	}
	return toModelDomain(preferProviderUpstreamRoute(provider, upstreamModel, rows)), nil
}

// preferProviderUpstreamRoute 在同一上游存在多条对外名路由时选出稳定代表路由。
func preferProviderUpstreamRoute(provider account.Provider, upstreamModel string, rows []modelRouteModel) modelRouteModel {
	preferred := model.PreferUpstreamRoute(provider, upstreamModel, mapModelRows(rows))
	for _, row := range rows {
		if row.ID == preferred.ID {
			return row
		}
	}
	return rows[0]
}

func (r *ModelRepository) HasSuccessfulAccountSync(ctx context.Context, accountID uint64) (bool, error) {
	var row struct{ AccountID uint64 }
	err := r.db.db.WithContext(ctx).Model(&accountModelSyncStateModel{}).
		Select("account_id").
		Where("account_id = ? AND last_success_at IS NOT NULL", accountID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return row.AccountID > 0, err
}

// ListStaleAccountSyncIDs 返回模型能力快照缺失或过期的启用账号，不扫描已禁用账号。
func (r *ModelRepository) ListStaleAccountSyncIDs(ctx context.Context, before time.Time, limit int) ([]uint64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Joins("LEFT JOIN account_model_sync_states AS sync ON sync.account_id = account.id").
		Where("account.enabled = ? AND account.auth_status = ?", true, account.AuthStatusActive).
		Where("sync.last_success_at IS NULL OR sync.last_success_at < ?", before.UTC()).
		Order("sync.last_success_at ASC, account.id ASC").
		Limit(limit).
		Scan(&ids).Error
	return ids, err
}

func (r *ModelRepository) MergeRoutes(ctx context.Context, provider account.Provider, values []model.Route) error {
	changed := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockModelNamespaces(tx, provider); err != nil {
			return err
		}
		var existing []modelRouteModel
		if err := tx.Select(modelRouteNameColumns).Where("provider = ?", provider).Find(&existing).Error; err != nil {
			return err
		}
		type managedRouteKey struct {
			publicID   string
			capability model.Capability
		}
		publicIDs := make(map[managedRouteKey]bool, len(existing))
		for _, row := range existing {
			if row.Origin != string(model.OriginManual) {
				publicIDs[managedRouteKey{publicID: row.PublicID, capability: model.Capability(row.Capability)}] = true
			}
		}
		rows := make([]modelRouteModel, 0, len(values))
		for _, value := range values {
			publicID, ok := model.NormalizePublicID(provider, value.PublicID)
			if !ok || value.Provider != provider || strings.TrimSpace(value.UpstreamModel) == "" || value.Capability == "" || (value.Origin != model.OriginDiscovered && value.Origin != model.OriginCatalog) {
				return fmt.Errorf("发现模型路由包含无效条目")
			}
			capability := value.Capability
			// Managed routes are idempotent by canonical public_id and capability.
			// Manual targets with the same name remain independent pool members.
			key := managedRouteKey{publicID: publicID, capability: capability}
			if publicIDs[key] {
				continue
			}
			// A discovered model whose canonical public ID is already reserved as
			// another route compatibility alias cannot be inserted. Skip it and
			// keep the rest of the batch: aborting here lets one historical rename
			// permanently hide every future model of this provider.
			if err := ensureModelPublicIDNotAlias(tx, publicID, 0); err != nil {
				if errors.Is(err, repository.ErrConflict) {
					continue
				}
				return err
			}
			publicIDs[key] = true
			rows = append(rows, modelRouteModel{PublicID: publicID, Provider: string(provider), UpstreamModel: value.UpstreamModel, Capability: string(capability), Origin: string(value.Origin), NameSource: string(model.NameSourceGenerated), Enabled: value.Enabled})
		}
		if len(rows) > 0 {
			// Concurrent discovery is guarded by the managed-route partial unique index.
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 200)
			changed = result.Error == nil && result.RowsAffected > 0
			return result.Error
		}
		return nil
	})
	if err == nil && changed {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
	}
	return err
}

func (r *ModelRepository) ReplaceProviderRoutes(ctx context.Context, provider account.Provider, values []model.Route) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockModelNamespaces(tx, provider); err != nil {
			return err
		}
		normalizedValues := make([]model.Route, len(values))
		for index, value := range values {
			publicID, ok := model.NormalizePublicID(provider, value.PublicID)
			if !ok {
				return fmt.Errorf("模型路由目录包含无效公开 ID %q", value.PublicID)
			}
			value.PublicID = publicID
			if strings.TrimSpace(value.PublicID) == "" || strings.TrimSpace(value.UpstreamModel) == "" || value.Provider != provider || value.Capability == "" {
				return fmt.Errorf("模型路由目录包含无效条目")
			}
			normalizedValues[index] = value
		}
		values = normalizedValues
		var existing []modelRouteModel
		if err := tx.Select(modelRouteNameColumns).Where("provider = ?", provider).Find(&existing).Error; err != nil {
			return err
		}

		type catalogRouteKey struct {
			value      string
			capability model.Capability
		}
		byPublicID := make(map[catalogRouteKey]modelRouteModel, len(existing))
		byUpstreamNonManual := make(map[catalogRouteKey][]modelRouteModel, len(existing))
		for _, row := range existing {
			if row.Origin == string(model.OriginManual) {
				continue
			}
			publicKey := catalogRouteKey{value: row.PublicID, capability: model.Capability(row.Capability)}
			if current, exists := byPublicID[publicKey]; !exists || row.ID < current.ID {
				byPublicID[publicKey] = row
			}
			upstreamKey := catalogRouteKey{value: row.UpstreamModel, capability: model.Capability(row.Capability)}
			byUpstreamNonManual[upstreamKey] = append(byUpstreamNonManual[upstreamKey], row)
		}
		matched := make(map[int]modelRouteModel, len(values))
		usedIDs := make(map[uint64]bool, len(values))
		for index, value := range values {
			publicKey := catalogRouteKey{value: value.PublicID, capability: value.Capability}
			if row, ok := byPublicID[publicKey]; ok && !usedIDs[row.ID] {
				matched[index] = row
				usedIDs[row.ID] = true
				continue
			}
			// 仅回退匹配非手动路由，避免把手动别名路由改写成目录项。
			candidates := byUpstreamNonManual[catalogRouteKey{value: value.UpstreamModel, capability: value.Capability}]
			var chosen modelRouteModel
			found := false
			for _, candidate := range candidates {
				if usedIDs[candidate.ID] {
					continue
				}
				if !found || candidate.ID < chosen.ID {
					chosen = candidate
					found = true
				}
			}
			if found {
				matched[index] = chosen
				usedIDs[chosen.ID] = true
			}
		}
		for _, row := range existing {
			if usedIDs[row.ID] || row.Origin == string(model.OriginManual) {
				continue
			}
			if err := tx.Delete(&modelRouteModel{}, row.ID).Error; err != nil {
				return err
			}
		}
		nameSources, err := reconcileCatalogNames(tx, matched, values)
		if err != nil {
			return err
		}
		// Temporary values allow public IDs or upstream identifiers to be swapped
		// while stable route IDs and key permissions survive.
		for _, row := range matched {
			updates := map[string]any{
				"public_id":      fmt.Sprintf("__grok2api_reconcile_%d", row.ID),
				"upstream_model": fmt.Sprintf("__grok2api_upstream_reconcile_%d", row.ID),
			}
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
				return mapError(err)
			}
		}
		for index, value := range values {
			updates := map[string]any{
				"public_id":      value.PublicID,
				"upstream_model": value.UpstreamModel,
				"capability":     value.Capability,
				"origin":         model.OriginCatalog,
			}
			if row, ok := matched[index]; ok {
				updates["name_source"] = nameSources[row.ID]
				if err := tx.Model(&modelRouteModel{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
					return mapError(err)
				}
				if row.UpstreamModel != value.UpstreamModel {
					if err := renameAccountModelCapability(tx, provider, row.UpstreamModel, value.UpstreamModel); err != nil {
						return err
					}
				}
				continue
			}
			row := modelRouteModel{PublicID: value.PublicID, Provider: string(provider), UpstreamModel: value.UpstreamModel, Capability: string(value.Capability), Origin: string(model.OriginCatalog), NameSource: string(model.NameSourceGenerated), Enabled: value.Enabled}
			if err := tx.Create(&row).Error; err != nil {
				return mapError(err)
			}
		}
		return nil
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
	}
	return err
}

func renameAccountModelCapability(tx *gorm.DB, provider account.Provider, oldModel, newModel string) error {
	providerAccounts := tx.Model(&accountModel{}).Select("id").Where("provider = ?", provider)
	duplicates := tx.Model(&accountModelCapabilityModel{}).
		Select("account_id").
		Where("upstream_model = ? AND account_id IN (?)", newModel, providerAccounts)
	if err := tx.Where("upstream_model = ? AND account_id IN (?) AND account_id IN (?)", oldModel, providerAccounts, duplicates).
		Delete(&accountModelCapabilityModel{}).Error; err != nil {
		return err
	}
	return tx.Model(&accountModelCapabilityModel{}).
		Where("upstream_model = ? AND account_id IN (?)", oldModel, providerAccounts).
		Update("upstream_model", newModel).Error
}

func (r *ModelRepository) Create(ctx context.Context, value model.Route, accountIDs []uint64) (model.Route, error) {
	publicID, ok := model.NormalizePublicID(value.Provider, value.PublicID)
	if !ok {
		return model.Route{}, fmt.Errorf("模型路由公开 ID 无效")
	}
	value.PublicID = publicID
	row := modelRouteModel{
		PublicID: value.PublicID, Provider: string(value.Provider), UpstreamModel: value.UpstreamModel,
		Capability: string(value.Capability), Origin: string(model.OriginManual), NameSource: string(model.NameSourceManual), Enabled: value.Enabled,
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockModelNamespaces(tx, value.Provider); err != nil {
			return err
		}
		if err := ensureModelPublicIDNotAlias(tx, value.PublicID, 0); err != nil {
			return err
		}
		if err := tx.Create(&row).Error; err != nil {
			return mapError(err)
		}
		if err := replaceModelRouteAccounts(tx, row.ID, accountIDs); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return model.Route{}, err
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: value.Provider, UpstreamModel: value.UpstreamModel})
	if len(accountIDs) > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationModelBindingChanged, Provider: value.Provider, UpstreamModel: value.UpstreamModel})
	}
	return r.Get(ctx, row.ID)
}

func (r *ModelRepository) Patch(ctx context.Context, id uint64, patch model.RoutePatch) (model.Route, error) {
	var existing modelRouteModel
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Select("provider").Where("id = ?", id).First(&existing).Error; err != nil {
			return mapError(err)
		}
		if err := lockModelNamespaces(tx, account.Provider(existing.Provider)); err != nil {
			return err
		}

		// PostgreSQL serializes edits and deletion on the route; SQLite opens
		// an immediate write transaction. Preserve aliases from this current row.
		if err := tx.Select(modelRouteNameColumns).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&existing).Error; err != nil {
			return mapError(err)
		}
		updates := make(map[string]any, 2)
		if patch.PublicID != nil {
			publicID, ok := model.NormalizePublicID(account.Provider(existing.Provider), *patch.PublicID)
			if !ok {
				return fmt.Errorf("模型路由公开 ID 无效")
			}
			if err := ensureModelPublicIDNotAlias(tx, publicID, existing.ID); err != nil {
				return err
			}
			if existing.PublicID != publicID {
				if err := preserveModelRouteAlias(tx, existing.PublicID, existing.ID, model.NameSourceManual); err != nil {
					return err
				}
			}
			updates["public_id"] = publicID
			updates["name_source"] = model.NameSourceManual
		}
		if patch.Enabled != nil {
			updates["enabled"] = *patch.Enabled
		}
		if len(updates) > 0 {
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				return mapError(err)
			}
		}
		if patch.PublicID != nil {
			// An explicit administrator claim also restores a retained historical
			// relationship to this same route, including a catalog-replaced edge.
			if err := tx.Model(&modelRouteAliasModel{}).Where("alias = ? AND model_route_id = ?", updates["public_id"], id).
				Updates(map[string]any{"name_source": model.NameSourceManual, "replaced_by_catalog": false}).Error; err != nil {
				return mapError(err)
			}
		}
		if patch.AccountIDs != nil {
			return replaceModelRouteAccounts(tx, id, *patch.AccountIDs)
		}
		return nil
	})
	if err != nil {
		return model.Route{}, err
	}
	if patch.PublicID != nil || patch.Enabled != nil || patch.AccountIDs != nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: account.Provider(existing.Provider), UpstreamModel: existing.UpstreamModel})
	}
	if patch.AccountIDs != nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationModelBindingChanged, Provider: account.Provider(existing.Provider), UpstreamModel: existing.UpstreamModel})
	}
	return r.Get(ctx, id)
}

func (r *ModelRepository) Delete(ctx context.Context, id uint64) error {
	var existing modelRouteModel
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&existing, id).Error; err != nil {
			return mapError(err)
		}
		if err := lockModelNamespaces(tx, account.Provider(existing.Provider)); err != nil {
			return err
		}
		result := tx.Delete(&modelRouteModel{}, id)
		if result.Error != nil {
			return mapError(result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: account.Provider(existing.Provider)})
	}
	return err
}

func (r *ModelRepository) DeleteMany(ctx context.Context, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var existing []modelRouteModel
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id IN ?", ids).Find(&existing).Error; err != nil {
			return err
		}
		if err := lockModelNamespaces(tx, modelRouteProviders(existing)...); err != nil {
			return err
		}
		result := tx.Where("id IN ?", ids).Delete(&modelRouteModel{})
		deleted = result.RowsAffected
		return mapError(result.Error)
	})
	if err == nil && deleted > 0 {
		providers := make(map[account.Provider]bool)
		for _, row := range existing {
			providers[account.Provider(row.Provider)] = true
		}
		for provider := range providers {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
		}
	}
	return deleted, err
}

func replaceModelRouteAccounts(tx *gorm.DB, routeID uint64, accountIDs []uint64) error {
	if err := tx.Where("model_route_id = ?", routeID).Delete(&modelRouteAccountModel{}).Error; err != nil {
		return err
	}
	if len(accountIDs) == 0 {
		return nil
	}
	rows := make([]modelRouteAccountModel, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		rows = append(rows, modelRouteAccountModel{ModelRouteID: routeID, AccountID: accountID})
	}
	return tx.CreateInBatches(rows, 200).Error
}

func (r *ModelRepository) UpdateManyEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	providers := make(map[account.Provider]struct{})
	var updated int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []modelRouteModel
		if err := tx.Where("id IN ?", ids).Find(&existing).Error; err != nil {
			return err
		}
		if err := lockModelNamespaces(tx, modelRouteProviders(existing)...); err != nil {
			return err
		}

		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Order("id ASC").Find(&existing).Error; err != nil {
			return err
		}
		for _, row := range existing {
			providers[account.Provider(row.Provider)] = struct{}{}
		}
		result := tx.Model(&modelRouteModel{}).Where("id IN ? AND enabled <> ?", ids, enabled).Update("enabled", enabled)
		updated = result.RowsAffected
		return result.Error
	})
	if err == nil && updated > 0 {
		for provider := range providers {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
		}
	}
	return updated, err
}

func (r *ModelRepository) availableRoutes(query *gorm.DB) *gorm.DB {
	predicate, args := modelCapabilityPredicate()
	return query.Where(predicate, args...).Where(availableRoutePredicate, true, account.AuthStatusActive)
}

type availabilityRow struct {
	RouteID           uint64
	SupportedAccounts int
	SyncedAccounts    int
	TotalAccounts     int
	LastSyncedUnix    sql.NullInt64
}

func (r *ModelRepository) lastSyncedAggregateExpression() string {
	if r.db.dialect == "postgres" {
		return "CAST(MAX(EXTRACT(EPOCH FROM sync.last_success_at)) AS BIGINT)"
	}
	return "MAX(unixepoch(sync.last_success_at))"
}

// annotateAvailability fills per-route account support counts. The original
// single query joined every route against the full same-provider account
// universe (routes × accounts); with very large account pools the cross
// product reaches millions of intermediate rows plus five COUNT(DISTINCT)
// aggregates, pushing the models list latency into multi-second territory at
// scale. The rewrite decomposes the same
// semantics: unbound routes (the common case) read pre-aggregated per-provider
// totals and per-(provider, upstream_model) capability counts computed in one
// pass each, while manually bound routes keep the original join restricted to
// the (rare) bound route IDs.
func (r *ModelRepository) annotateAvailability(ctx context.Context, values []model.Route) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	var bindings []modelRouteAccountModel
	if err := r.db.db.WithContext(ctx).Where("model_route_id IN ?", ids).Order("model_route_id ASC, account_id ASC").Find(&bindings).Error; err != nil {
		return err
	}
	boundByRoute := make(map[uint64][]uint64, len(values))
	boundIDs := make([]uint64, 0)
	boundSeen := make(map[uint64]bool, len(bindings))
	for _, binding := range bindings {
		if !boundSeen[binding.ModelRouteID] {
			boundSeen[binding.ModelRouteID] = true
			boundIDs = append(boundIDs, binding.ModelRouteID)
		}
		boundByRoute[binding.ModelRouteID] = append(boundByRoute[binding.ModelRouteID], binding.AccountID)
	}
	unboundIDs := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if !boundSeen[id] {
			unboundIDs = append(unboundIDs, id)
		}
	}
	byID := make(map[uint64]availabilityRow, len(values))
	if len(unboundIDs) > 0 {
		rows, err := r.unboundAvailabilityRows(ctx, unboundIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			byID[row.RouteID] = row
		}
	}
	if len(boundIDs) > 0 {
		rows, err := r.boundAvailabilityRows(ctx, boundIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			byID[row.RouteID] = row
		}
	}
	for index := range values {
		row := byID[values[index].ID]
		values[index].SupportedAccounts = row.SupportedAccounts
		if !model.SupportsCapability(values[index].Provider, values[index].UpstreamModel, values[index].Capability) {
			values[index].SupportedAccounts = 0
		}
		values[index].SyncedAccounts = row.SyncedAccounts
		values[index].TotalAccounts = row.TotalAccounts
		values[index].BoundAccountIDs = boundByRoute[values[index].ID]
		if row.LastSyncedUnix.Valid {
			lastSyncedAt := time.Unix(row.LastSyncedUnix.Int64, 0).UTC()
			values[index].LastSyncedAt = &lastSyncedAt
		}
	}
	return nil
}

// consoleCatalogRoutePredicate is the catalog-half static-support check for
// console routes (the provider equality is enforced by the caller's CASE
// branch).
const consoleCatalogRoutePredicate = `(
	route.origin = 'catalog'
	OR EXISTS (
		SELECT 1 FROM model_routes console_catalog_route
		WHERE console_catalog_route.provider = route.provider
			AND console_catalog_route.upstream_model = route.upstream_model
			AND console_catalog_route.capability = route.capability
			AND console_catalog_route.origin = 'catalog'
	)
)`

// webCatalogRoutePredicate is the catalog-half static-support check for the
// four basic media upstream models (provider and model-list checks stay in the
// CASE).
const webCatalogRoutePredicate = `(
	route.origin = 'catalog'
	OR EXISTS (
		SELECT 1 FROM model_routes web_catalog_route
		WHERE web_catalog_route.provider = route.provider
			AND web_catalog_route.upstream_model = route.upstream_model
			AND web_catalog_route.capability = route.capability
			AND web_catalog_route.origin = 'catalog'
	)
)`

// unboundAvailabilityRows computes availability for routes without manual
// account bindings using three one-pass aggregates:
//
//	provider_totals        enabled+active account totals/synced/last-synced per provider
//	capability_counts      capability-backed support per (provider, upstream_model)
//	paid_capability_counts grok_build paid/super accounts per upstream_model
//
// supported decomposition per route (identical set semantics to the original
// join): console-catalog routes and web basic-media catalog routes count the
// whole enabled+active provider pool (the static support is unconditional on
// the account side and subsumes capability matches); every other route counts
// capability matches, and grok_build routes additionally count the paid/super
// pool when a paid peer exposes the capability — the union size is
// caps + paidTotal − paidCaps by inclusion-exclusion, matching the original
// COUNT(DISTINCT) dedup.
func (r *ModelRepository) unboundAvailabilityRows(ctx context.Context, routeIDs []uint64) ([]availabilityRow, error) {
	var paidTotal int64
	if err := r.db.db.WithContext(ctx).Raw(`
		SELECT COUNT(*) FROM provider_accounts account
		WHERE account.provider = 'grok_build' AND account.enabled = TRUE AND account.auth_status = ?
			AND (`+modelAccountBuildSuperPredicate+`)
	`, account.AuthStatusActive).Scan(&paidTotal).Error; err != nil {
		return nil, err
	}
	var rows []availabilityRow
	err := r.db.db.WithContext(ctx).Raw(fmt.Sprintf(`
		WITH provider_totals AS (
			SELECT account.provider AS provider,
				COUNT(*) AS total_accounts,
				COUNT(sync.last_success_at) AS synced_accounts,
				%s AS last_synced_unix
			FROM provider_accounts account
			LEFT JOIN account_model_sync_states sync ON sync.account_id = account.id
			WHERE account.enabled = TRUE AND account.auth_status = ?
			GROUP BY account.provider
		),
		capability_counts AS (
			SELECT account.provider AS provider, capability.upstream_model AS upstream_model, COUNT(*) AS supported
			FROM account_model_capabilities capability
			JOIN provider_accounts account ON account.id = capability.account_id
			WHERE account.enabled = TRUE AND account.auth_status = ?
			GROUP BY account.provider, capability.upstream_model
		),
		paid_capability_counts AS (
			SELECT capability.upstream_model AS upstream_model, COUNT(*) AS paid_with_capability
			FROM account_model_capabilities capability
			JOIN provider_accounts account ON account.id = capability.account_id
			WHERE account.provider = 'grok_build' AND account.enabled = TRUE AND account.auth_status = ?
				AND (`+modelAccountBuildSuperPredicate+`)
			GROUP BY capability.upstream_model
		)
		SELECT route.id AS route_id,
			CASE
				WHEN route.provider = 'grok_console' AND `+consoleCatalogRoutePredicate+`
					THEN COALESCE(provider_totals.total_accounts, 0)
				WHEN route.provider = 'grok_web' AND route.upstream_model IN ('grok-imagine-image-quality', 'grok-imagine-image-2.0', 'imagine-image-edit', 'grok-imagine-video') AND `+webCatalogRoutePredicate+`
					THEN COALESCE(provider_totals.total_accounts, 0)
				WHEN route.provider = 'grok_build' AND COALESCE(paid_capability_counts.paid_with_capability, 0) > 0
					THEN COALESCE(capability_counts.supported, 0) + ? - COALESCE(paid_capability_counts.paid_with_capability, 0)
				ELSE COALESCE(capability_counts.supported, 0)
			END AS supported_accounts,
			COALESCE(provider_totals.synced_accounts, 0) AS synced_accounts,
			COALESCE(provider_totals.total_accounts, 0) AS total_accounts,
			provider_totals.last_synced_unix AS last_synced_unix
		FROM model_routes route
		LEFT JOIN provider_totals ON provider_totals.provider = route.provider
		LEFT JOIN capability_counts ON capability_counts.provider = route.provider AND capability_counts.upstream_model = route.upstream_model
		LEFT JOIN paid_capability_counts ON paid_capability_counts.upstream_model = route.upstream_model
		WHERE route.id IN ?
	`, r.lastSyncedAggregateExpression()), account.AuthStatusActive, account.AuthStatusActive, account.AuthStatusActive, paidTotal, routeIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// boundAvailabilityRows keeps the original route × account join for manually
// bound routes, where supported/total must intersect the explicit binding set.
// Bound routes are rare (bulk imports leave routes unbound), so the heavy join
// runs only for those IDs.
func (r *ModelRepository) boundAvailabilityRows(ctx context.Context, routeIDs []uint64) ([]availabilityRow, error) {
	var rows []availabilityRow
	err := r.db.db.WithContext(ctx).Raw(fmt.Sprintf(`
		SELECT route.id AS route_id,
			COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND binding.account_id IS NOT NULL THEN account.id END) AS supported_accounts,
			COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND binding.account_id IS NOT NULL AND sync.last_success_at IS NOT NULL THEN account.id END) AS synced_accounts,
			COUNT(DISTINCT binding.account_id) AS total_accounts,
			%s AS last_synced_unix
		FROM model_routes route
		LEFT JOIN provider_accounts account ON account.provider = route.provider
		LEFT JOIN model_route_accounts binding ON binding.model_route_id = route.id AND binding.account_id = account.id
		LEFT JOIN account_model_sync_states sync ON sync.account_id = account.id
		WHERE route.id IN ?
		GROUP BY route.id
	`, r.lastSyncedAggregateExpression()), account.AuthStatusActive, account.AuthStatusActive, routeIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func mapModelRows(rows []modelRouteModel) []model.Route {
	out := make([]model.Route, 0, len(rows))
	for _, row := range rows {
		out = append(out, toModelDomain(row))
	}
	return out
}
