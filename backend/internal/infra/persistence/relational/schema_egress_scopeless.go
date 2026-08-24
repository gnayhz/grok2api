package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gorm.io/gorm"
)

// egressRoutingTargetRow is the JSON shape of one persisted routing target.
type egressRoutingTargetRow struct {
	Mode   string `json:"mode"`
	NodeID uint64 `json:"nodeId,omitempty"`
	PoolID uint64 `json:"poolId,omitempty"`
}

// egressRoutingPayload is the persisted routing configuration: the default
// (总出口) target, per-scope targets, and per-traffic-class targets.
type egressRoutingPayload struct {
	Default *egressRoutingTargetRow           `json:"default,omitempty"`
	Scopes  map[string]egressRoutingTargetRow `json:"scopes,omitempty"`
	Classes map[string]egressRoutingTargetRow `json:"classes,omitempty"`
}

// legacyEgressRouting carries the routing configuration captured from the
// pre-refactor columns through the schema rebuild so it can be restored as
// the unified routing JSON once the new column exists.
type legacyEgressRouting struct {
	captured bool
	payload  string
}

// legacyEgressOperationsColumns lists the retired operations-config columns.
var legacyEgressOperationsColumns = []string{
	"auto_assign_enabled", "auto_balance_enabled", "assignment_interval_seconds",
	"build_fallback_mode", "build_fallback_node_id",
	"web_fallback_mode", "web_fallback_node_id",
	"console_fallback_mode", "console_fallback_node_id",
	"web_asset_fallback_mode", "web_asset_fallback_node_id",
	"console_asset_fallback_mode", "console_asset_fallback_node_id",
	"route_rules", "subscription_proxy_migration_completed", "proxy_profile_migration_completed",
	"encrypted_subscription_proxy_url",
}

// legacyEgressColumnsByTable lists every retired column per table: resource
// scopes, account→proxy bindings, and shared proxy profiles.
var legacyEgressColumnsByTable = map[string][]string{
	"provider_accounts":           {"egress_node_id", "egress_assignment_mode", "egress_pool_id", "egress_assigned_at"},
	"egress_nodes":                {"scope", "pool_id", "account_capacity", "proxy_profile_id"},
	"egress_pools":                {"scope"},
	"egress_subscription_sources": {"scope", "default_account_capacity", "pool_id"},
}

// dropLegacyEgressResourceColumns removes the columns behind the retired
// concepts — resource scopes, account bindings, shared proxy profiles, and
// the split fallback/route-rule configuration. Routing is captured first so
// the operator's exit decisions survive as the unified routing JSON. It runs
// before AutoMigrate inside the foreign-key-disabled session: SQLite column
// drops rebuild tables.
func (d *Database) dropLegacyEgressResourceColumns(ctx context.Context) (legacyEgressRouting, error) {
	db := d.db.WithContext(ctx)
	migrator := db.Migrator()
	if !migrator.HasTable("egress_nodes") {
		return legacyEgressRouting{}, nil
	}
	result := legacyEgressRouting{}
	if migrator.HasTable("egress_operations_config") && migrator.HasColumn("egress_operations_config", "build_fallback_mode") {
		payload, err := d.captureLegacyEgressRouting(ctx)
		if err != nil {
			return legacyEgressRouting{}, err
		}
		result = legacyEgressRouting{captured: true, payload: payload}
	}
	// The single-pool binding backfills pool membership before its column dies.
	// The members table may itself be missing on older databases; AutoMigrate
	// creates it right after, and there is nothing to backfill then.
	if migrator.HasTable("egress_pool_members") && migrator.HasColumn("egress_nodes", "pool_id") {
		if err := db.Exec("INSERT INTO egress_pool_members (pool_id, node_id) SELECT pool_id, id FROM egress_nodes WHERE pool_id IS NOT NULL AND pool_id <> 0 AND NOT EXISTS (SELECT 1 FROM egress_pool_members m WHERE m.pool_id = egress_nodes.pool_id AND m.node_id = egress_nodes.id)").Error; err != nil {
			return legacyEgressRouting{}, fmt.Errorf("迁移代理池成员: %w", err)
		}
	}
	// Pool names become globally unique once the scope column is gone; rename
	// duplicates before the new unique index is created.
	if migrator.HasColumn("egress_pools", "scope") {
		if err := db.Exec("UPDATE egress_pools SET name = name || '-' || CAST(id AS TEXT) WHERE id NOT IN (SELECT MIN(id) FROM egress_pools GROUP BY name)").Error; err != nil {
			return legacyEgressRouting{}, fmt.Errorf("合并同名代理池: %w", err)
		}
	}
	for table, columns := range legacyEgressColumnsByTable {
		if !migrator.HasTable(table) {
			continue
		}
		if err := d.dropEgressLegacyColumns(ctx, table, columns); err != nil {
			return legacyEgressRouting{}, err
		}
	}
	if migrator.HasTable("egress_operations_config") {
		if err := d.dropEgressLegacyColumns(ctx, "egress_operations_config", legacyEgressOperationsColumns); err != nil {
			return legacyEgressRouting{}, err
		}
	}
	// The profile table only goes away once nothing references it.
	if migrator.HasTable("egress_proxy_profiles") && !migrator.HasColumn("egress_nodes", "proxy_profile_id") {
		if err := migrator.DropTable("egress_proxy_profiles"); err != nil {
			return legacyEgressRouting{}, fmt.Errorf("删除共享代理配置表: %w", err)
		}
		slog.Info("egress proxy profile table dropped")
	}
	return result, nil
}

// dropEgressLegacyColumns performs guarded column drops for one table. The
// sqlite migrator needs a model value (a bare table string yields a nil
// schema inside DropColumn's table rebuild), so each legacy table maps to its
// current model here.
func (d *Database) dropEgressLegacyColumns(ctx context.Context, table string, columns []string) error {
	var model any
	switch table {
	case "provider_accounts":
		model = &accountModel{}
	case "egress_nodes":
		model = &egressNodeModel{}
	case "egress_pools":
		model = &egressPoolModel{}
	case "egress_subscription_sources":
		model = &egressSubscriptionSourceModel{}
	case "egress_operations_config":
		model = &egressOperationsConfigModel{}
	default:
		return fmt.Errorf("未知的出口遗留表: %s", table)
	}
	migrator := d.db.WithContext(ctx).Migrator()
	// Foreign keys and table-level CHECKs referencing the dropped columns must
	// go first: SQLite rebuilds the table from the stored DDL and rejects a
	// CREATE TABLE whose constraint still names a dropped column.
	for _, constraint := range legacyEgressConstraintsByTable[table] {
		if migrator.HasConstraint(model, constraint) {
			if err := migrator.DropConstraint(model, constraint); err != nil {
				return fmt.Errorf("删除 %s.%s: %w", table, constraint, err)
			}
			slog.Info("egress legacy constraint dropped", "table", table, "constraint", constraint)
		}
	}
	for _, column := range columns {
		if !migrator.HasColumn(model, column) {
			continue
		}
		if err := migrator.DropColumn(model, column); err != nil {
			return fmt.Errorf("删除 %s.%s: %w", table, column, err)
		}
		slog.Info("egress legacy column dropped", "table", table, "column", column)
	}
	return nil
}

// legacyEgressConstraintsByTable lists stored constraints that reference the
// retired columns and therefore must not survive the rebuild.
var legacyEgressConstraintsByTable = map[string][]string{
	"provider_accounts":           {"fk_provider_accounts_egress_node", "chk_accounts_egress_assignment_mode"},
	"egress_nodes":                {"chk_egress_nodes_specific_scope", "chk_egress_nodes_capacity", "fk_egress_nodes_proxy_profile"},
	"egress_pools":                {"chk_egress_pools_scope", "uidx_egress_pools_scope_name"},
	"egress_subscription_sources": {"chk_egress_subscription_sources_scope", "chk_egress_subscription_sources_capacity"},
	"egress_operations_config":    {"chk_egress_operations_config_assignment_interval", "chk_egress_operations_config_subscription_proxy"},
}

// restoreLegacyEgressRouting writes the captured routing decisions into the
// new unified routing column after AutoMigrate created it.
func (d *Database) restoreLegacyEgressRouting(ctx context.Context, legacy legacyEgressRouting) error {
	if !legacy.captured || strings.TrimSpace(legacy.payload) == "" {
		return nil
	}
	return d.db.WithContext(ctx).Exec("UPDATE egress_operations_config SET routing = ? WHERE id = 1", legacy.payload).Error
}

// captureLegacyEgressRouting converts the legacy per-scope fallback columns
// and build route rules into the unified routing payload. Scope fallbacks
// become scope targets (asset scopes fold into their parent family), route
// rules become class targets.
func (d *Database) captureLegacyEgressRouting(ctx context.Context) (string, error) {
	var row struct {
		BuildFallbackMode          string
		BuildFallbackNodeID        uint64
		WebFallbackMode            string
		WebFallbackNodeID          uint64
		ConsoleFallbackMode        string
		ConsoleFallbackNodeID      uint64
		WebAssetFallbackMode       string
		WebAssetFallbackNodeID     uint64
		ConsoleAssetFallbackMode   string
		ConsoleAssetFallbackNodeID uint64
		RouteRules                 string
	}
	err := d.db.WithContext(ctx).Table("egress_operations_config").Where("id = ?", 1).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取旧版出口回退配置: %w", err)
	}
	payload := egressRoutingPayload{}
	legacyScopeTarget := func(mode string, nodeID uint64) (egressRoutingTargetRow, bool) {
		switch mode {
		case "fixed":
			return egressRoutingTargetRow{Mode: "node", NodeID: nodeID}, true
		case "direct":
			return egressRoutingTargetRow{Mode: "direct"}, true
		default:
			return egressRoutingTargetRow{}, false
		}
	}
	// Asset scopes fold into their parent family; the parent target wins when
	// both were configured.
	targets := map[string]egressRoutingTargetRow{}
	if target, ok := legacyScopeTarget(row.BuildFallbackMode, row.BuildFallbackNodeID); ok {
		targets["grok_build"] = target
	}
	if target, ok := legacyScopeTarget(row.WebAssetFallbackMode, row.WebAssetFallbackNodeID); ok {
		targets["grok_web"] = target
	}
	if target, ok := legacyScopeTarget(row.WebFallbackMode, row.WebFallbackNodeID); ok {
		targets["grok_web"] = target
	}
	if target, ok := legacyScopeTarget(row.ConsoleAssetFallbackMode, row.ConsoleAssetFallbackNodeID); ok {
		targets["grok_console"] = target
	}
	if target, ok := legacyScopeTarget(row.ConsoleFallbackMode, row.ConsoleFallbackNodeID); ok {
		targets["grok_console"] = target
	}
	if len(targets) > 0 {
		payload.Scopes = targets
	}
	if strings.TrimSpace(row.RouteRules) != "" {
		var rules []struct {
			Scope        string `json:"scope"`
			Class        string `json:"class"`
			TargetMode   string `json:"targetMode"`
			TargetNodeID uint64 `json:"targetNodeId"`
			TargetPoolID uint64 `json:"targetPoolId"`
			Enabled      bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(row.RouteRules), &rules); err != nil {
			slog.Warn("egress legacy route rules payload is corrupt; skipping", "error", err)
			rules = nil
		}
		classes := map[string]egressRoutingTargetRow{}
		for _, rule := range rules {
			if !rule.Enabled || rule.Class == "" {
				continue
			}
			var target egressRoutingTargetRow
			switch rule.TargetMode {
			case "fixed":
				if rule.TargetNodeID == 0 {
					continue
				}
				target = egressRoutingTargetRow{Mode: "node", NodeID: rule.TargetNodeID}
			case "direct":
				target = egressRoutingTargetRow{Mode: "direct"}
			case "pool":
				if rule.TargetPoolID == 0 {
					continue
				}
				target = egressRoutingTargetRow{Mode: "pool", PoolID: rule.TargetPoolID}
			default:
				continue
			}
			classes[rule.Class] = target
		}
		if len(classes) > 0 {
			payload.Classes = classes
		}
	}
	if payload.Default == nil && len(payload.Scopes) == 0 && len(payload.Classes) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码出口路由配置: %w", err)
	}
	return string(encoded), nil
}

// unmarshalEgressRouting decodes the persisted routing JSON. A corrupt
// payload degrades to the automatic schedule instead of locking the whole
// operations config read.
func unmarshalEgressRouting(raw string) (egressRoutingPayload, error) {
	payload := egressRoutingPayload{}
	if strings.TrimSpace(raw) == "" {
		return payload, nil
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return egressRoutingPayload{}, err
	}
	return payload, nil
}

func marshalEgressRouting(payload egressRoutingPayload) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}
