package relational

import (
	"context"
	"testing"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Corrupted routing payloads must degrade to the automatic schedule instead
// of failing the whole operations config read, which would lock administrator
// repair.
func TestEgressOperationsConfigCorruptRoutingDegrades(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)

	// Seed a valid row first.
	config := egress.OperationsConfig{
		ProbeProvider: egress.ProbeProviderCloudflare, ProbeIntervalSeconds: 900,
		DefaultTarget: egress.RoutingTarget{Mode: egress.RoutingTargetDirect},
	}
	saved, err := nodes.SaveEgressOperationsConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DefaultTarget.Mode != egress.RoutingTargetDirect {
		t.Fatalf("saved default target = %#v", saved.DefaultTarget)
	}

	// Corrupt the routing column the way a manual SQL edit would.
	if err := database.db.WithContext(ctx).Exec("UPDATE egress_operations_config SET routing = ? WHERE id = 1", "not-json{").Error; err != nil {
		t.Fatal(err)
	}

	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatalf("corrupt routing must not fail the config read: %v", err)
	}
	if got := stored.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("corrupt routing should degrade to auto, got %#v", got)
	}
	// The probe scheduling layer keeps working.
	if stored.ProbeProvider != egress.ProbeProviderCloudflare || stored.ProbeIntervalSeconds != 900 {
		t.Fatalf("probe config lost: %#v", stored)
	}
}

// Sticky per-account proxy templates must be rejected as fixed routing
// targets because their exit rotates with the caller identity.
func TestServiceRejectsStickyProxyAsRoutingTarget(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	encrypted, err := cipher.Encrypt("http://{account}:pass@proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	sticky, err := nodes.CreateEgressNode(ctx, egress.Node{Name: "sticky-template", Enabled: true, EncryptedProxyURL: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	service := egressapp.NewService(nodes, cipher)

	_, err = service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900,
		DefaultTarget:        &egressapp.RoutingTargetInput{Mode: egress.RoutingTargetNode, NodeID: sticky.ID},
	})
	if err == nil {
		t.Fatal("sticky template node must be rejected as a fixed routing target")
	}
}

// Editing a node that serves as a fixed routing target into an unusable
// state must be rejected.
func TestServiceRejectsRoutingTargetDegradation(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "guard-target")
	service := egressapp.NewService(nodes, cipher)

	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900,
		ClassTargets: map[egress.TrafficClass]egressapp.RoutingTargetInput{
			egress.TrafficClassInference: {Mode: egress.RoutingTargetNode, NodeID: target.ID},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Disabling the target must be rejected.
	if _, err := service.Update(ctx, target.ID, egressapp.Input{Name: "guard-target", Enabled: false}); err == nil {
		t.Fatal("disable of a routing target must be rejected")
	}
	// An unrelated edit stays allowed.
	if _, err := service.Update(ctx, target.ID, egressapp.Input{Name: "guard-target-renamed", Enabled: true}); err != nil {
		t.Fatalf("unrelated edit should pass: %v", err)
	}
}

// Editing a routing target into a sticky per-account template must be
// rejected, mirroring the save-time sticky validation.
func TestServiceRejectsTargetEditIntoStickyTemplate(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "plain-target")
	service := egressapp.NewService(nodes, cipher)

	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900,
		DefaultTarget:        &egressapp.RoutingTargetInput{Mode: egress.RoutingTargetNode, NodeID: target.ID},
	}); err != nil {
		t.Fatal(err)
	}

	sticky := "http://{account}:pass@proxy.example:8080"
	if _, err := service.Update(ctx, target.ID, egressapp.Input{Name: "plain-target", Enabled: true, ProxyURL: &sticky}); err == nil {
		t.Fatal("editing a routing target into a sticky template must be rejected")
	}
}

// Subscription sync must strip routing targets whose node disappeared; the
// application-layer hygiene pass re-saves the filtered config.
func TestSubscriptionSyncClearsStaleRoutingTarget(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	source, err := nodes.CreateEgressSource(ctx, egress.SubscriptionSource{
		Name: "source", Enabled: true, RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://subscription.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, []egress.Node{{
		Name: "subscription", Enabled: true, SourceID: source.ID,
		SourceKey: "one", EncryptedProxyURL: encryptedProxy, Health: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	listed, err := nodes.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil || len(listed) != 1 {
		t.Fatalf("subscription nodes=%#v err=%v", listed, err)
	}
	config := egress.DefaultOperationsConfig()
	config.ScopeTargets = map[egress.Scope]egress.RoutingTarget{
		egress.ScopeBuild: {Mode: egress.RoutingTargetNode, NodeID: listed[0].ID},
	}
	if _, err := nodes.SaveEgressOperationsConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	// 订阅同步移除该节点后,目标被清空。
	if _, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("stale subscription routing target = %#v", got)
	}
}
