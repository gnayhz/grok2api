package relational

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// TestPostgresEgressLegacySchemaUpgrade 在真实 PostgreSQL 上复刻"重构前旧库"
// （节点/池/订阅源带 scope 与账号绑定列、ops 配置为分作用域回退列 + route_rules
// JSON、账号表带出口绑定外键），运行当前 InitializeSchema，验证：
//   - 旧列/旧约束全部删除，资源行无损；
//   - 节点单池绑定(pool_id)回填为 egress_pool_members 成员；
//   - 同名跨作用域池在全局唯一名重建前被改名，唯一索引恢复；
//   - 旧回退列与 route_rules 被捕获并恢复为统一 routing JSON，资产作用域折叠
//     进所属族、禁用/无效规则被丢弃；
//   - 换 IP/降智列升级后仓储窄方法往返读写正常。
//
// 环境变量 TEST_POSTGRES_ADMIN_DSN（或 TEST_POSTGRES_DSN）提供实例，未配置时跳过。
func TestPostgresEgressLegacySchemaUpgrade(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if err := database.db.WithContext(ctx).Exec(sql, args...).Error; err != nil {
			t.Fatalf("exec %s: %v", sql, err)
		}
	}

	// 当前形态的种子数据：两个节点、一个账号、一个订阅源；池在旧列退化后建。
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	unique := time.Now().UTC().Format("20060102150405.000000000")
	fixedNode := createHealthyEgressNode(t, ctx, nodes, cipher, "pg-legacy-fixed-"+unique)
	otherNode := createHealthyEgressNode(t, ctx, nodes, cipher, "pg-legacy-other-"+unique)
	accounts := NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "pg-legacy", SourceKey: "pg-legacy-" + unique,
		EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO egress_subscription_sources (name, enabled, encrypted_url, refresh_interval_seconds, last_sync_imported, last_sync_error, encrypted_proxy_url, created_at, updated_at) VALUES ('pg-legacy-" + unique + "', true, 'enc', 900, 0, '', '', now(), now())")

	// ---- 退化为重构前旧库 ----
	exec("ALTER TABLE egress_nodes ADD COLUMN scope text NOT NULL DEFAULT 'grok_build', ADD COLUMN pool_id bigint NOT NULL DEFAULT 0, ADD COLUMN account_capacity int NOT NULL DEFAULT 0, ADD COLUMN proxy_profile_id bigint")
	exec("ALTER TABLE egress_nodes ADD CONSTRAINT chk_egress_nodes_capacity CHECK (account_capacity >= 0)")
	exec("ALTER TABLE egress_pools ADD COLUMN scope text NOT NULL DEFAULT 'grok_build'")
	// 全局唯一名索引不存在于旧库（唯一性按 scope+name），先撤掉才能造出同名池。
	exec("DROP INDEX IF EXISTS uidx_egress_pools_name")
	exec("ALTER TABLE egress_pools DROP CONSTRAINT IF EXISTS uidx_egress_pools_name")
	exec("INSERT INTO egress_pools (name, enabled, strategy, fallback_mode, fallback_pool_id, rotation_cursor_node_id, scope, created_at, updated_at) VALUES ('pg-shared-pool-" + unique + "', true, 'affinity', 'none', 0, 0, 'grok_build', now(), now())")
	exec("INSERT INTO egress_pools (name, enabled, strategy, fallback_mode, fallback_pool_id, rotation_cursor_node_id, scope, created_at, updated_at) VALUES ('pg-shared-pool-" + unique + "', true, 'affinity', 'none', 0, 0, 'grok_console', now(), now())")
	var buildPoolID, consolePoolID uint64
	if err := database.db.WithContext(ctx).Raw("SELECT id FROM egress_pools WHERE scope = 'grok_build' AND name = ?", "pg-shared-pool-"+unique).Scan(&buildPoolID).Error; err != nil || buildPoolID == 0 {
		t.Fatalf("build pool id=%d err=%v", buildPoolID, err)
	}
	if err := database.db.WithContext(ctx).Raw("SELECT id FROM egress_pools WHERE scope = 'grok_console' AND name = ?", "pg-shared-pool-"+unique).Scan(&consolePoolID).Error; err != nil || consolePoolID == 0 {
		t.Fatalf("console pool id=%d err=%v", consolePoolID, err)
	}
	// provider_accounts 绑定列是活功能（模型保留、升级后必须在且数据无损），
	// 无需造旧形状；egress_nodes/pools/sources 的旧列退化已在上方完成。
	exec("UPDATE egress_nodes SET pool_id = ?, scope = 'grok_build', account_capacity = 5 WHERE id = ?", buildPoolID, fixedNode.ID)
	exec("UPDATE provider_accounts SET egress_assignment_mode = 'auto' WHERE id = ?", credential.ID)
	exec("ALTER TABLE egress_subscription_sources ADD COLUMN scope text NOT NULL DEFAULT 'grok_build', ADD COLUMN default_account_capacity int NOT NULL DEFAULT 0, ADD COLUMN pool_id bigint NOT NULL DEFAULT 0")
	routeRules := `[{"scope":"grok_build","class":"inference","targetMode":"pool","targetPoolId":` + strconv.FormatUint(buildPoolID, 10) + `,"enabled":true},` +
		`{"scope":"grok_build","class":"billing","targetMode":"fixed","targetNodeId":` + strconv.FormatUint(otherNode.ID, 10) + `,"enabled":false},` +
		`{"scope":"grok_build","class":"model_sync","targetMode":"fixed","targetNodeId":0,"enabled":true}]`
	exec("ALTER TABLE egress_operations_config ADD COLUMN build_fallback_mode text, ADD COLUMN build_fallback_node_id bigint, ADD COLUMN web_fallback_mode text, ADD COLUMN web_fallback_node_id bigint, ADD COLUMN console_fallback_mode text, ADD COLUMN console_fallback_node_id bigint, ADD COLUMN web_asset_fallback_mode text, ADD COLUMN web_asset_fallback_node_id bigint, ADD COLUMN route_rules text, ADD COLUMN auto_assign_enabled boolean NOT NULL DEFAULT false")
	exec("INSERT INTO egress_operations_config (id, probe_provider, probe_interval_seconds, routing, updated_at, build_fallback_mode, build_fallback_node_id, web_fallback_mode, web_fallback_node_id, console_fallback_mode, console_fallback_node_id, web_asset_fallback_mode, web_asset_fallback_node_id, route_rules) VALUES (1, 'cloudflare', 900, '', now(), 'fixed', ?, 'auto', 0, 'auto', 0, 'direct', 0, ?)", fixedNode.ID, routeRules)

	// ---- 升级到当前 schema ----
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("legacy upgrade failed on PostgreSQL: %v", err)
	}
	// 真实升级以新进程（新连接池）连库；同时规避 pgx 在同连接上 DDL 后的
	// prepared-statement 缓存（cached plan must not change result type）。
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = OpenPostgres(ctx, os.Getenv("TEST_POSTGRES_DSN"), 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// 升级后的校验全部走新连接（真实升级语义）：重建仓储绑定。
	nodes = NewEgressRepository(database)
	accounts = NewAccountRepository(database)

	migrator := database.db.WithContext(ctx).Migrator()
	for table, columns := range map[string][]string{
		"egress_nodes":                {"scope", "pool_id", "account_capacity", "proxy_profile_id"},
		"egress_pools":                {"scope"},
		"egress_subscription_sources": {"scope", "default_account_capacity", "pool_id"},
		"egress_operations_config":    {"build_fallback_mode", "build_fallback_node_id", "web_asset_fallback_mode", "route_rules", "auto_assign_enabled"},
	} {
		for _, column := range columns {
			if migrator.HasColumn(table, column) {
				t.Errorf("legacy column %s.%s survived the upgrade", table, column)
			}
		}
	}
	// 账号-出口绑定列是活功能面：升级后必须仍然存在且数据无损。
	for _, column := range []string{"egress_node_id", "egress_assignment_mode", "egress_assigned_at"} {
		if !migrator.HasColumn("provider_accounts", column) {
			t.Errorf("live binding column provider_accounts.%s missing after upgrade", column)
		}
	}

	// 单池绑定回填为成员关系。
	var memberCount int64
	if err := database.db.WithContext(ctx).Model(&egressPoolMemberModel{}).Where("pool_id = ? AND node_id = ?", buildPoolID, fixedNode.ID).Count(&memberCount).Error; err != nil || memberCount != 1 {
		t.Fatalf("pool membership backfill count=%d err=%v", memberCount, err)
	}

	// 同名池改名 + 全局唯一索引恢复。
	var poolNames []string
	if err := database.db.WithContext(ctx).Model(&egressPoolModel{}).Where("id IN ?", []uint64{buildPoolID, consolePoolID}).Order("id").Pluck("name", &poolNames).Error; err != nil || len(poolNames) != 2 {
		t.Fatalf("pool names after upgrade = %v err=%v", poolNames, err)
	}
	if poolNames[0] == poolNames[1] {
		t.Fatalf("duplicate pool names not de-duplicated: %v", poolNames)
	}
	if !migrator.HasIndex(&egressPoolModel{}, "uidx_egress_pools_name") {
		t.Error("global unique pool name index was not restored after upgrade")
	}

	// 账号与订阅源行无损。
	if _, err := accounts.Get(ctx, credential.ID); err != nil {
		t.Fatalf("account row lost during upgrade: %v", err)
	}
	var sourceCount int64
	if err := database.db.WithContext(ctx).Model(&egressSubscriptionSourceModel{}).Where("name = ?", "pg-legacy-"+unique).Count(&sourceCount).Error; err != nil || sourceCount != 1 {
		t.Fatalf("subscription source row lost during upgrade: count=%d err=%v", sourceCount, err)
	}

	// 旧路由决策恢复为统一 routing JSON。
	config, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if target := config.TargetFor(egressdomain.ScopeBuild, egressdomain.TrafficClassInference); target.Mode != egressdomain.RoutingTargetPool || target.PoolID != buildPoolID {
		t.Fatalf("class rule inference = %+v, want pool %d", target, buildPoolID)
	}
	// 禁用规则不得产生类别条目;billing 类落到作用域目标(节点)是阶梯语义而非泄漏。
	if _, ok := config.ClassTargets[egressdomain.TrafficClassBilling]; ok {
		t.Fatal("disabled legacy billing rule leaked into class targets")
	}
	if _, ok := config.ClassTargets[egressdomain.TrafficClassModelSync]; ok {
		t.Fatal("legacy rule with targetNodeId=0 leaked into class targets")
	}
	if target := config.TargetFor(egressdomain.ScopeBuild, egressdomain.TrafficClassCredential); target.Mode != egressdomain.RoutingTargetNode || target.NodeID != fixedNode.ID {
		t.Fatalf("build fallback -> scope target = %+v, want node %d", target, fixedNode.ID)
	}
	// 类别规则横跨作用域:inference 在 Web 下仍是池(上已验证);无类别规则的
	// 流量才落到作用域目标, 由此验证 web_asset direct 折叠进 grok_web。
	if target := config.TargetFor(egressdomain.ScopeWeb, egressdomain.TrafficClassCredential); target.Mode != egressdomain.RoutingTargetDirect {
		t.Fatalf("web asset fallback folded into scope = %+v, want direct", target)
	}

	// 换 IP/降智列升级后的仓储窄方法往返。
	until := time.Now().Add(time.Hour).UTC()
	degradedAt := time.Now().UTC()
	if err := nodes.UpdateEgressNodeQualityState(ctx, fixedNode.ID, 0.5, 2, &until, egressdomain.LastErrorExitIPQuality, 1, &degradedAt); err != nil {
		t.Fatal(err)
	}
	rotatedAt := degradedAt.Add(time.Minute)
	if err := nodes.UpdateEgressNodeRotationState(ctx, fixedNode.ID, &rotatedAt, 1, "canary degraded"); err != nil {
		t.Fatal(err)
	}
	updated, err := nodes.GetEgressNode(ctx, fixedNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastError != egressdomain.LastErrorExitIPQuality || updated.DegradeCount != 1 || updated.RotationAttempts != 1 || updated.LastRotationError != "canary degraded" || updated.LastRotatedAt == nil || updated.LastDegradedAt == nil {
		t.Fatalf("postgres rotation round-trip mismatch: %#v", updated)
	}
	if err := nodes.UpdateEgressNodeRotationState(ctx, 99999, nil, 0, ""); err == nil || !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing node error = %v, want ErrNotFound", err)
	}

	// 再跑一次 InitializeSchema 必须幂等（升级后的库重复重启）。
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("re-initialization after upgrade is not idempotent: %v", err)
	}
	var routingRaw string
	if err := database.db.WithContext(ctx).Raw("SELECT routing FROM egress_operations_config WHERE id = 1").Scan(&routingRaw).Error; err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Scopes map[string]json.RawMessage `json:"scopes"`
	}
	if err := json.Unmarshal([]byte(routingRaw), &payload); err != nil {
		t.Fatalf("persisted routing is not valid JSON: %v (%q)", err, routingRaw)
	}
}
