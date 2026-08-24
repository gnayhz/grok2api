package relational

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func routingTargetTestConfig(defaultTarget *egress.RoutingTarget, scopes map[egress.Scope]egress.RoutingTarget, classes map[egress.TrafficClass]egress.RoutingTarget) egress.OperationsConfig {
	config := egress.OperationsConfig{
		ProbeProvider: egress.ProbeProviderCloudflare, ProbeIntervalSeconds: 900,
		ScopeTargets: scopes, ClassTargets: classes,
	}
	if defaultTarget != nil {
		config.DefaultTarget = *defaultTarget
	}
	return config
}

// 路由目标(默认/作用域/流量类别)作为统一 routing JSON 落库并原样读回。
func TestEgressOperationsConfigPersistsRoutingTargets(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "rule-target")

	config := routingTargetTestConfig(
		nil,
		map[egress.Scope]egress.RoutingTarget{
			egress.ScopeBuild: {Mode: egress.RoutingTargetNode, NodeID: target.ID},
			egress.ScopeWeb:   {Mode: egress.RoutingTargetDirect},
		},
		map[egress.TrafficClass]egress.RoutingTarget{
			egress.TrafficClassBilling: {Mode: egress.RoutingTargetNode, NodeID: target.ID},
			egress.TrafficClassVideo:   {Mode: egress.RoutingTargetDirect},
		},
	)
	saved, err := nodes.SaveEgressOperationsConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetNode || got.NodeID != target.ID {
		t.Fatalf("saved build scope target = %+v", got)
	}
	if got := saved.TargetFor(egress.ScopeWeb, egress.TrafficClassInference); got.Mode != egress.RoutingTargetDirect {
		t.Fatalf("saved web scope target = %+v", got)
	}
	if got := saved.TargetFor(egress.ScopeConsole, egress.TrafficClassBilling); got.Mode != egress.RoutingTargetNode || got.NodeID != target.ID {
		t.Fatalf("saved billing class target = %+v", got)
	}

	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetNode || got.NodeID != target.ID {
		t.Fatalf("stored build scope target = %+v", got)
	}
	if got := stored.TargetFor(egress.ScopeConsole, egress.TrafficClassBilling); got.Mode != egress.RoutingTargetNode || got.NodeID != target.ID {
		t.Fatalf("stored billing class target = %+v", got)
	}
	if got := stored.TargetFor(egress.ScopeConsole, egress.TrafficClassVideo); got.Mode != egress.RoutingTargetDirect {
		t.Fatalf("stored video class target = %+v", got)
	}
	// 路由 JSON 落在 routing 列,而不是旧的 route_rules 列。
	var raw string
	if err := database.db.WithContext(ctx).Raw("SELECT routing FROM egress_operations_config WHERE id = 1").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"\"scopes\"", "\"classes\"", "grok_build"} {
		if !strings.Contains(raw, fragment) {
			t.Fatalf("routing JSON %q missing %s", raw, fragment)
		}
	}
}

func TestEgressOperationsConfigRoundTripsEmptyRouting(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)

	saved, err := nodes.SaveEgressOperationsConfig(ctx, routingTargetTestConfig(nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("empty routing target = %+v", got)
	}

	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("stored empty routing target = %+v", got)
	}
}

// 存储层校验:固定节点目标必须存在且可服务(CanNodeServeFixedTarget)。
func TestEgressOperationsConfigRejectsUnsafeRoutingTarget(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "missing-target")

	// Unknown node id.
	_, err := nodes.SaveEgressOperationsConfig(ctx, routingTargetTestConfig(
		&egress.RoutingTarget{Mode: egress.RoutingTargetNode, NodeID: target.ID + 9999}, nil, nil))
	if !errors.Is(err, repository.ErrEgressRoutingNodeInUse) {
		t.Fatalf("missing node err = %v, want ErrEgressRoutingNodeInUse", err)
	}

	// 池目标必须真实存在。
	_, err = nodes.SaveEgressOperationsConfig(ctx, routingTargetTestConfig(
		&egress.RoutingTarget{Mode: egress.RoutingTargetPool, PoolID: 404}, nil, nil))
	if !errors.Is(err, repository.ErrEgressRoutingInvalid) {
		t.Fatalf("missing pool err = %v, want ErrEgressRoutingInvalid", err)
	}

	// 代理池节点不能再当固定出口目标(出口由网关轮换)。
	poolNode := createHealthyEgressNode(t, ctx, nodes, cipher, "gateway")
	poolNode.ProxyPool = true
	if _, err := nodes.UpdateEgressNode(ctx, poolNode); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.SaveEgressOperationsConfig(ctx, routingTargetTestConfig(
		&egress.RoutingTarget{Mode: egress.RoutingTargetNode, NodeID: poolNode.ID}, nil, nil)); !errors.Is(err, repository.ErrEgressRoutingNodeInUse) {
		t.Fatalf("proxy-pool target err = %v, want ErrEgressRoutingNodeInUse", err)
	}
}

// 引用保护:禁用被引用节点被拒绝;删除节点只清掉指向它的目标。
func TestRoutingTargetReferenceIsProtectedAndClearedOnDelete(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "rule-target")
	other := createHealthyEgressNode(t, ctx, nodes, cipher, "rule-other")

	config := routingTargetTestConfig(nil, nil, map[egress.TrafficClass]egress.RoutingTarget{
		egress.TrafficClassInference: {Mode: egress.RoutingTargetNode, NodeID: target.ID},
		egress.TrafficClassModelSync: {Mode: egress.RoutingTargetNode, NodeID: other.ID},
	})
	if _, err := nodes.SaveEgressOperationsConfig(ctx, config); err != nil {
		t.Fatal(err)
	}

	// Disabling a referenced node is rejected.
	if _, err := nodes.UpdateEgressNodesEnabled(ctx, []uint64{target.ID}, false); !errors.Is(err, repository.ErrEgressRoutingNodeInUse) {
		t.Fatalf("disable referenced node err = %v, want ErrEgressRoutingNodeInUse", err)
	}

	// Deleting the referenced node removes only its target.
	if err := nodes.DeleteEgressNode(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("inference target should fall back to auto after deletion: %+v", got)
	}
	if got := stored.TargetFor(egress.ScopeBuild, egress.TrafficClassModelSync); got.Mode != egress.RoutingTargetNode || got.NodeID != other.ID {
		t.Fatalf("unrelated target should survive deletion: %+v", got)
	}
}

// 禁用节点上的保存被拒绝后,已存配置保持不变(事务内锁定重检)。
func TestEgressOperationsConfigRechecksTargetInsideTransaction(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "transaction-target")

	if _, err := nodes.UpdateEgressNodesEnabled(ctx, []uint64{target.ID}, false); err != nil {
		t.Fatal(err)
	}
	config := egress.DefaultOperationsConfig()
	config.DefaultTarget = egress.RoutingTarget{Mode: egress.RoutingTargetNode, NodeID: target.ID}
	config.UpdatedAt = time.Now().UTC()
	if _, err := nodes.SaveEgressOperationsConfig(ctx, config); !errors.Is(err, repository.ErrEgressRoutingNodeInUse) {
		t.Fatalf("disabled target save error = %v", err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("rejected target was persisted: %+v", got)
	}
}

// 未配置默认行时 Get 返回默认配置,且首行由 lockEgressOperationsConfig 建立。
func TestEgressOperationsConfigLockCreatesDefaultRow(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)

	config, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if config.ProbeProvider != egress.ProbeProviderCloudflare || config.ProbeIntervalSeconds != 900 {
		t.Fatalf("default config = %#v", config)
	}
	// Reads do not persist a row; the default row is created lazily by the
	// save-time lock.
	var count int64
	if err := database.db.WithContext(ctx).Model(&egressOperationsConfigModel{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("operations config rows = %d before any save, want 0", count)
	}
	saved, err := nodes.SaveEgressOperationsConfig(ctx, config)
	if err != nil || saved.ProbeProvider != egress.ProbeProviderCloudflare {
		t.Fatalf("default save = %#v err=%v", saved, err)
	}
	if err := database.db.WithContext(ctx).Model(&egressOperationsConfigModel{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("operations config rows = %d after save, want 1", count)
	}
}
