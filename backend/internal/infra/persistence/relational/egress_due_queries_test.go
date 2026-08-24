package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// TestListDueEgressQueriesCompile 锁定两条到期查询的 SQL 语法:历史上
// encrypted_*_url <> ' 缺闭合引号导致 SQL 字面量吞掉后续语句,维护循环
// (到期探测 + 到期订阅同步)每分钟静默失败。真实 SQLite 执行即验证。
func TestListDueEgressQueriesCompile(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "due-queries.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	repo := &EgressRepository{db: database}
	if err := database.db.Exec(
		`INSERT INTO egress_nodes (name, enabled, encrypted_proxy_url, health, probe_status, created_at, updated_at) VALUES ('due-node', 1, 'enc', 1, 'unknown', ?, ?)`,
		now, now,
	).Error; err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := database.db.Exec(
		`INSERT INTO egress_subscription_sources (name, enabled, encrypted_url, refresh_interval_seconds, created_at, updated_at) VALUES ('due-source', 1, 'enc', 900, ?, ?)`,
		now, now,
	).Error; err != nil {
		t.Fatalf("seed source: %v", err)
	}

	nodes, err := repo.ListDueEgressNodes(ctx, now, 15*time.Minute, 32)
	if err != nil {
		t.Fatalf("ListDueEgressNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "due-node" {
		t.Fatalf("due nodes = %+v, want seeded due-node", nodes)
	}
	sources, err := repo.ListDueEgressSources(ctx, now, 3)
	if err != nil {
		t.Fatalf("ListDueEgressSources: %v", err)
	}
	if len(sources) != 1 || sources[0].Name != "due-source" {
		t.Fatalf("due sources = %+v, want seeded due-source", sources)
	}
}

// TestProbeSchedulingNoFlappingUnderFaultInjection 锁定探活调度的防抖语义:
// (1) 探测后 interval 内不再入选(退避), 到期后重新入选;
// (2) 探测失败只写 probe_* 观测字段, 不改 health/cooldown/failure_count/
//
//	last_error——调度状态不被观测路径抖动(调度排除只来自真实流量
//	Feedback 或质量隔离);
//
// (3) 探测成功清除传输层冷却(恢复路径);
// (4) 未探测过的节点优先(NULLS FIRST), 禁用/无代理节点永不入选。
func TestProbeSchedulingNoFlappingUnderFaultInjection(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "probe-flapping.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	repo := &EgressRepository{db: database}
	seed := func(name, proxy string, enabled bool, lastProbed *time.Time, cooldown *time.Time, failures int, lastError string) {
		t.Helper()
		if err := database.db.Exec(
			`INSERT INTO egress_nodes (name, enabled, encrypted_proxy_url, health, failure_count, cooldown_until, last_error, probe_status, last_probed_at, created_at, updated_at)
			 VALUES (?, ?, ?, 1, ?, ?, ?, 'unknown', ?, ?, ?)`,
			name, enabled, proxy, failures, cooldown, lastError, lastProbed, now, now,
		).Error; err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	old := now.Add(-30 * time.Minute)
	recent := now.Add(-1 * time.Minute)
	cooldownUntil := now.Add(20 * time.Minute)
	seed("dead-proxy", "enc", true, &old, &cooldownUntil, 3, egress.LastErrorTransport) // A: 传输冷却中, 已到期
	seed("fresh-node", "enc", true, nil, nil, 0, "")                                    // B: 从未探测
	seed("fresh-higher", "enc", true, nil, nil, 0, "")                                  // F: 从未探测, id 更大
	seed("disabled-node", "enc", false, nil, nil, 0, "")
	seed("no-proxy", "", true, nil, nil, 0, "")
	seed("recent-node", "enc", true, &recent, nil, 0, "")

	// 初始到期集合: A(到期) + B + F(从未探测, NULLS FIRST 后按 id);
	// 禁用/无代理/刚探测过的排除。
	due, err := repo.ListDueEgressNodes(ctx, now, 15*time.Minute, 32)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(due))
	for _, node := range due {
		names = append(names, node.Name)
	}
	if len(names) != 3 || names[0] != "fresh-node" || names[1] != "fresh-higher" || names[2] != "dead-proxy" {
		t.Fatalf("initial due set = %v, want [fresh-node fresh-higher dead-proxy] (never-probed first by id)", names)
	}

	// 探测失败: 只写观测字段, 调度状态不动(不抖动)。
	unhealthyAt := now
	if err := repo.UpdateEgressNodeProbe(ctx, due[0].ID, "enc", egress.ProbeResult{
		Status: egress.ProbeStatusUnhealthy, TestedAt: unhealthyAt, LatencyMS: 7, Error: "dial timeout",
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := repo.GetEgressNode(ctx, due[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Health != 1 || fresh.FailureCount != 0 || fresh.CooldownUntil != nil || fresh.LastError != "" {
		t.Fatalf("unhealthy probe must not flap scheduling state: %+v", fresh)
	}
	if fresh.ProbeStatus != egress.ProbeStatusUnhealthy || fresh.LastProbedAt == nil || !fresh.LastProbedAt.Equal(unhealthyAt) {
		t.Fatalf("probe observation fields not persisted: %+v", fresh)
	}
	// interval 内不再入选(退避), 到期后重新入选。
	if again, err := repo.ListDueEgressNodes(ctx, now.Add(14*time.Minute), 15*time.Minute, 32); err != nil {
		t.Fatal(err)
	} else {
		for _, node := range again {
			if node.ID == fresh.ID {
				t.Fatal("node re-selected before probe interval elapsed (probe hot-loop)")
			}
		}
	}
	if again, err := repo.ListDueEgressNodes(ctx, now.Add(16*time.Minute), 15*time.Minute, 32); err != nil {
		t.Fatal(err)
	} else {
		found := false
		for _, node := range again {
			if node.ID == fresh.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("node not re-selected after probe interval elapsed")
		}
	}

	// 探测成功: 清除传输层冷却(恢复路径), 其他冷却保留。
	deadID := due[2].ID
	if err := repo.UpdateEgressNodeProbe(ctx, deadID, "enc", egress.ProbeResult{
		Status: egress.ProbeStatusHealthy, TestedAt: now, LatencyMS: 42, ExitIP: "198.51.100.9",
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := repo.GetEgressNode(ctx, deadID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CooldownUntil != nil || recovered.FailureCount != 0 || recovered.LastError != "" || recovered.Health != 1 {
		t.Fatalf("healthy probe must clear transport cooldown: %+v", recovered)
	}
	if recovered.ExitIP != "198.51.100.9" || recovered.ProbeStatus != egress.ProbeStatusHealthy {
		t.Fatalf("healthy probe observation not persisted: %+v", recovered)
	}
}
