package relational

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// legacyEgressSchemaSQL reproduces the exact pre-refactor DDL captured from a live database so the drop/migrate path runs against the real legacy shape.
const legacyEgressSchemaSQL = "CREATE TABLE `egress_operations_config` (`id` integer PRIMARY KEY AUTOINCREMENT,`probe_provider` text NOT NULL DEFAULT \"cloudflare\",`probe_interval_seconds` integer NOT NULL DEFAULT 900,`auto_assign_enabled` numeric NOT NULL DEFAULT false,`auto_balance_enabled` numeric NOT NULL DEFAULT false,`assignment_interval_seconds` integer NOT NULL DEFAULT 300,`encrypted_subscription_proxy_url` text NOT NULL DEFAULT \"\",`build_fallback_mode` text NOT NULL DEFAULT \"none\",`build_fallback_node_id` integer NOT NULL DEFAULT 0,`web_fallback_mode` text NOT NULL DEFAULT \"none\",`web_fallback_node_id` integer NOT NULL DEFAULT 0,`console_fallback_mode` text NOT NULL DEFAULT \"none\",`console_fallback_node_id` integer NOT NULL DEFAULT 0,`web_asset_fallback_mode` text NOT NULL DEFAULT \"none\",`web_asset_fallback_node_id` integer NOT NULL DEFAULT 0,`console_asset_fallback_mode` text NOT NULL DEFAULT \"none\",`console_asset_fallback_node_id` integer NOT NULL DEFAULT 0,`updated_at` datetime NOT NULL, `subscription_proxy_migration_completed` numeric NOT NULL DEFAULT false, `proxy_profile_migration_completed` numeric NOT NULL DEFAULT false, `route_rules` text NOT NULL DEFAULT \"\",CONSTRAINT `chk_egress_operations_config_subscription_proxy` CHECK (length(encrypted_subscription_proxy_url) <= 65536),CONSTRAINT `chk_egress_operations_config_probe_interval` CHECK (probe_interval_seconds BETWEEN 60 AND 86400),CONSTRAINT `chk_egress_operations_config_id` CHECK (id = 1),CONSTRAINT `chk_egress_operations_config_probe_provider` CHECK (probe_provider IN ('ipinfo','cloudflare')),CONSTRAINT `chk_egress_operations_config_assignment_interval` CHECK (assignment_interval_seconds BETWEEN 60 AND 86400))\nCREATE TABLE \"egress_subscription_sources\"  (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text NOT NULL,`scope` text NOT NULL,`enabled` numeric NOT NULL DEFAULT true,`encrypted_url` text NOT NULL DEFAULT \"\",`refresh_interval_seconds` integer NOT NULL DEFAULT 900,`default_account_capacity` integer NOT NULL DEFAULT 0,`last_synced_at` datetime,`next_sync_at` datetime,`last_sync_imported` integer NOT NULL DEFAULT 0,`last_sync_error` text NOT NULL DEFAULT \"\",`created_at` datetime NOT NULL,`updated_at` datetime NOT NULL,`encrypted_proxy_url` text NOT NULL DEFAULT \"\", `pool_id` integer,CONSTRAINT `chk_egress_subscription_sources_url` CHECK (length(encrypted_url) <= 65536),CONSTRAINT `chk_egress_subscription_sources_imported` CHECK (last_sync_imported >= 0),CONSTRAINT `chk_egress_subscription_sources_error` CHECK (length(last_sync_error) <= 512),CONSTRAINT `chk_egress_subscription_sources_name` CHECK (length(trim(name)) BETWEEN 1 AND 160),CONSTRAINT `chk_egress_subscription_sources_scope` CHECK (scope IN ('grok_build','grok_web','grok_console','grok_web_asset','grok_console_asset')),CONSTRAINT `chk_egress_subscription_sources_refresh` CHECK (refresh_interval_seconds BETWEEN 60 AND 86400),CONSTRAINT `chk_egress_subscription_sources_capacity` CHECK (default_account_capacity BETWEEN 0 AND 100000),CONSTRAINT `chk_egress_subscription_sources_proxy_url` CHECK (length(encrypted_proxy_url) <= 65536))\nCREATE TABLE `egress_proxy_profiles` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text NOT NULL,`encrypted_proxy_url` text NOT NULL,`created_at` datetime NOT NULL,`updated_at` datetime NOT NULL,CONSTRAINT `chk_egress_proxy_profiles_url` CHECK (length(encrypted_proxy_url) BETWEEN 1 AND 65536),CONSTRAINT `chk_egress_proxy_profiles_name` CHECK (length(trim(name)) BETWEEN 1 AND 160))\nCREATE TABLE \"egress_nodes\"  (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text NOT NULL,`scope` text NOT NULL,`enabled` numeric NOT NULL DEFAULT true,`proxy_pool` numeric NOT NULL DEFAULT false,`source_id` integer,`source_key` text NOT NULL DEFAULT \"\",`account_capacity` integer NOT NULL DEFAULT 0,`encrypted_proxy_url` text NOT NULL DEFAULT \"\",`user_agent` text NOT NULL DEFAULT \"\",`encrypted_cloudflare_cookie` text NOT NULL DEFAULT \"\",`clearance_refreshed_at` datetime,`clearance_fingerprint` text NOT NULL DEFAULT \"\",`clearance_binding_fingerprint` text NOT NULL DEFAULT \"\",`health` real NOT NULL DEFAULT 1,`failure_count` integer NOT NULL DEFAULT 0,`cooldown_until` datetime,`last_error` text,`probe_status` text NOT NULL DEFAULT \"unknown\",`last_probed_at` datetime,`probe_latency_ms` integer NOT NULL DEFAULT 0,`exit_ip` text NOT NULL DEFAULT \"\",`probe_error` text NOT NULL DEFAULT \"\",`probe_provider` text NOT NULL DEFAULT \"\",`ipv4_probe_status` text NOT NULL DEFAULT \"unknown\",`ipv4_last_probed_at` datetime,`ipv4_probe_latency_ms` integer NOT NULL DEFAULT 0,`ipv4_exit_ip` text NOT NULL DEFAULT \"\",`ipv4_probe_error` text NOT NULL DEFAULT \"\",`ipv6_probe_status` text NOT NULL DEFAULT \"unknown\",`ipv6_last_probed_at` datetime,`ipv6_probe_latency_ms` integer NOT NULL DEFAULT 0,`ipv6_exit_ip` text NOT NULL DEFAULT \"\",`ipv6_probe_error` text NOT NULL DEFAULT \"\",`created_at` datetime NOT NULL,`updated_at` datetime NOT NULL,`proxy_profile_id` integer,`encrypted_rotation_url` text NOT NULL DEFAULT \"\",`last_rotated_at` datetime,`rotation_attempts` integer NOT NULL DEFAULT 0,`last_rotation_error` text NOT NULL DEFAULT \"\",`degrade_count` integer NOT NULL DEFAULT 0,`last_degraded_at` datetime, `pool_id` integer, `rotation_enabled` numeric NOT NULL DEFAULT false,CONSTRAINT `fk_egress_nodes_source` FOREIGN KEY (`source_id`) REFERENCES `egress_subscription_sources`(`id`) ON DELETE SET NULL ON UPDATE CASCADE,CONSTRAINT `chk_egress_nodes_health` CHECK (health >= 0 AND health <= 1),CONSTRAINT `chk_egress_nodes_ipv4_probe_latency` CHECK (ipv4_probe_latency_ms >= 0),CONSTRAINT `chk_egress_nodes_cf_cookie` CHECK (length(encrypted_cloudflare_cookie) <= 65536),CONSTRAINT `chk_egress_nodes_clearance_binding_fingerprint` CHECK (length(clearance_binding_fingerprint) IN (0, 64)),CONSTRAINT `chk_egress_nodes_probe_status` CHECK (probe_status IN ('unknown','healthy','unhealthy')),CONSTRAINT `chk_egress_nodes_ipv6_exit_ip` CHECK (length(ipv6_exit_ip) <= 64),CONSTRAINT `chk_egress_nodes_source_key` CHECK (length(source_key) <= 64),CONSTRAINT `chk_egress_nodes_ipv6_probe_error` CHECK (length(ipv6_probe_error) <= 512),CONSTRAINT `chk_egress_nodes_ipv4_exit_ip` CHECK (length(ipv4_exit_ip) <= 64),CONSTRAINT `chk_egress_nodes_probe_provider` CHECK (probe_provider IN ('','ipinfo','cloudflare')),CONSTRAINT `chk_egress_nodes_ipv4_probe_status` CHECK (ipv4_probe_status IN ('unknown','healthy','unhealthy')),CONSTRAINT `chk_egress_nodes_failures` CHECK (failure_count >= 0),CONSTRAINT `chk_egress_nodes_capacity` CHECK (account_capacity BETWEEN 0 AND 100000),CONSTRAINT `chk_egress_nodes_specific_scope` CHECK (scope IN ('grok_build','grok_web','grok_console','grok_web_asset','grok_console_asset')),CONSTRAINT `chk_egress_nodes_user_agent` CHECK (length(user_agent) <= 512),CONSTRAINT `chk_egress_nodes_ipv6_probe_latency` CHECK (ipv6_probe_latency_ms >= 0),CONSTRAINT `chk_egress_nodes_exit_ip` CHECK (length(exit_ip) <= 64),CONSTRAINT `chk_egress_nodes_probe_error` CHECK (length(probe_error) <= 512),CONSTRAINT `chk_egress_nodes_name` CHECK (length(trim(name)) BETWEEN 1 AND 160),CONSTRAINT `chk_egress_nodes_ipv4_probe_error` CHECK (length(ipv4_probe_error) <= 512),CONSTRAINT `chk_egress_nodes_last_error` CHECK (length(last_error) <= 512),CONSTRAINT `chk_egress_nodes_ipv6_probe_status` CHECK (ipv6_probe_status IN ('unknown','healthy','unhealthy')),CONSTRAINT `chk_egress_nodes_proxy_url` CHECK (length(encrypted_proxy_url) <= 65536),CONSTRAINT `chk_egress_nodes_clearance_fingerprint` CHECK (length(clearance_fingerprint) IN (0, 64)),CONSTRAINT `chk_egress_nodes_probe_latency` CHECK (probe_latency_ms >= 0),CONSTRAINT `fk_egress_nodes_proxy_profile` FOREIGN KEY (`proxy_profile_id`) REFERENCES `egress_proxy_profiles`(`id`) ON DELETE RESTRICT ON UPDATE CASCADE,CONSTRAINT `chk_egress_nodes_degrade_count` CHECK (degrade_count >= 0),CONSTRAINT `chk_egress_nodes_rotation_error` CHECK (length(last_rotation_error) <= 512),CONSTRAINT `chk_egress_nodes_rotation_url` CHECK (length(encrypted_rotation_url) <= 65536),CONSTRAINT `chk_egress_nodes_rotation_attempts` CHECK (rotation_attempts >= 0))\nCREATE TABLE `egress_pools` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text NOT NULL,`scope` text NOT NULL,`enabled` numeric NOT NULL DEFAULT true,`fallback_mode` text NOT NULL DEFAULT \"none\",`fallback_pool_id` integer NOT NULL DEFAULT 0,`created_at` datetime NOT NULL,`updated_at` datetime NOT NULL,CONSTRAINT `chk_egress_pools_fallback_pool` CHECK ((fallback_mode <> 'pool' AND fallback_pool_id = 0) OR (fallback_mode = 'pool' AND fallback_pool_id > 0)),CONSTRAINT `chk_egress_pools_name` CHECK (length(trim(name)) BETWEEN 1 AND 160),CONSTRAINT `chk_egress_pools_scope` CHECK (scope IN ('grok_build','grok_web','grok_console','grok_web_asset','grok_console_asset')),CONSTRAINT `chk_egress_pools_fallback_mode` CHECK (fallback_mode IN ('none','pool','direct')))\nCREATE TABLE `egress_pool_members` (`pool_id` integer,`node_id` integer,PRIMARY KEY (`pool_id`,`node_id`))"

func seedLegacyEgressRows(t *testing.T, db *Database) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.db.Exec(`INSERT INTO egress_nodes (name, scope, enabled, encrypted_proxy_url, user_agent, health, probe_status, created_at, updated_at) VALUES (?, ?, 1, 'enc-a', '', 1, 'unknown', ?, ?)`, "node-a", "grok_build", now, now).Error; err != nil {
		t.Fatalf("seed node-a: %v", err)
	}
	if err := db.db.Exec(`INSERT INTO egress_nodes (name, scope, enabled, encrypted_proxy_url, user_agent, health, probe_status, created_at, updated_at) VALUES (?, ?, 1, 'enc-b', 'ua', 1, 'unknown', ?, ?)`, "node-b", "grok_web", now, now).Error; err != nil {
		t.Fatalf("seed node-b: %v", err)
	}
	if err := db.db.Exec(`INSERT INTO egress_pools (name, scope, enabled, fallback_mode, fallback_pool_id, created_at, updated_at) VALUES ('legacy-pool', 'grok_build', 1, 'none', 0, ?, ?)`, now, now).Error; err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	if err := db.db.Exec(`UPDATE egress_nodes SET pool_id = (SELECT MIN(id) FROM egress_pools) WHERE name = 'node-a'`).Error; err != nil {
		t.Fatalf("bind pool: %v", err)
	}
	if err := db.db.Exec("INSERT OR IGNORE INTO egress_operations_config (id, probe_provider, probe_interval_seconds, updated_at) VALUES (1, 'cloudflare', 900, ?)", now).Error; err != nil {
		t.Fatalf("seed config row: %v", err)
	}
	rule := "[{\"scope\":\"grok_build\",\"class\":\"billing\",\"targetMode\":\"pool\",\"targetPoolId\":1,\"enabled\":true}]"
	if err := db.db.Exec(`UPDATE egress_operations_config SET build_fallback_mode='fixed', build_fallback_node_id=(SELECT id FROM egress_nodes WHERE name='node-b'), web_fallback_mode='direct', route_rules=? WHERE id = 1`, rule).Error; err != nil {
		t.Fatalf("seed routing: %v", err)
	}
}

// TestInitializeSchemaDropsLegacyEgressColumns proves the refactor migration:
// legacy scope/binding/profile columns disappear, pool membership carries over
// from egress_nodes.pool_id, and legacy fallback/route-rule configuration is
// restored as the unified routing payload.
func TestInitializeSchemaDropsLegacyEgressColumns(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-egress.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, statement := range legacyDDLStatements() {
		_ = statement
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if err := database.db.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatalf("create legacy schema: %v", err)
		}
	}
	seedLegacyEgressRows(t, database)

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	migrator := database.db.Migrator()
	for _, stale := range []struct{ table, column string }{
		{"egress_nodes", "scope"}, {"egress_nodes", "account_capacity"}, {"egress_nodes", "proxy_profile_id"}, {"egress_nodes", "pool_id"},
		{"egress_pools", "scope"}, {"egress_subscription_sources", "scope"},
		{"egress_operations_config", "build_fallback_mode"}, {"egress_operations_config", "route_rules"},
	} {
		if migrator.HasColumn(stale.table, stale.column) {
			t.Errorf("stale column survived: %s.%s", stale.table, stale.column)
		}
	}
	if migrator.HasTable("egress_proxy_profiles") {
		t.Error("egress_proxy_profiles table survived")
	}

	// pool membership carried over from the legacy single-pool binding
	repo := NewEgressRepository(database)
	members, err := repo.EgressPoolMembers(ctx)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) != 1 || len(members[1]) != 1 {
		t.Errorf("membership not backfilled: %v", members)
	}

	// routing restored: build fixed fallback -> scope target; direct web fallback -> direct; rule -> class target
	var routingRaw string
	database.db.WithContext(ctx).Raw("SELECT routing FROM egress_operations_config WHERE id=1").Scan(&routingRaw)
	config, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if target, ok := config.ScopeTargets[egress.ScopeBuild]; !ok || target.Mode != egress.RoutingTargetNode || target.NodeID == 0 {
		t.Errorf("build scope target not restored: %+v", config.ScopeTargets)
	}
	if target, ok := config.ScopeTargets[egress.ScopeWeb]; !ok || target.Mode != egress.RoutingTargetDirect {
		t.Errorf("web scope target not restored: %+v", config.ScopeTargets)
	}
	if target, ok := config.ClassTargets[egress.TrafficClassBilling]; !ok || target.Mode != egress.RoutingTargetPool || target.PoolID != 1 {
		t.Errorf("billing class target not restored: %+v", config.ClassTargets)
	}

	// nodes survive with their proxy material intact
	nodes, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("node count = %d, want 2", len(nodes))
	}

	// second run must be a no-op
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("idempotent migrate: %v", err)
	}
}

// legacyDDLStatements splits the captured DDL on statement boundaries; naive semicolon splitting breaks on CHECK constraints that embed semicolons.
func legacyDDLStatements() []string {
	indices := regexp.MustCompile("CREATE TABLE").FindAllStringIndex(legacyEgressSchemaSQL, -1)
	statements := make([]string, 0, len(indices))
	for index, at := range indices {
		end := len(legacyEgressSchemaSQL)
		if index+1 < len(indices) {
			end = indices[index+1][0]
		}
		if trimmed := strings.TrimSpace(legacyEgressSchemaSQL[at[0]:end]); trimmed != "" {
			statements = append(statements, trimmed)
		}
	}
	return statements
}
