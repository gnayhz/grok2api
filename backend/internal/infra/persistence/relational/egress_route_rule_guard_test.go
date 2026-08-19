package relational

import (
	"context"
	"testing"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// Corrupted route_rules payloads must degrade to "no rules" instead of
// failing the whole operations config read, which would also break fallback
// configuration and lock administrator repair.
func TestEgressOperationsConfigCorruptRouteRulesDegrades(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)

	// Seed a valid row first.
	saved, err := nodes.SaveEgressOperationsConfig(ctx, routeRuleTestConfig([]egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetDirect, Enabled: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.RouteRules) != 1 {
		t.Fatalf("saved rules = %#v", saved.RouteRules)
	}

	// Corrupt the JSON column the way a manual SQL edit would.
	if err := database.db.WithContext(ctx).Exec("UPDATE egress_operations_config SET route_rules = ? WHERE id = 1", "not-json{").Error; err != nil {
		t.Fatal(err)
	}

	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatalf("corrupt route rules must not fail the config read: %v", err)
	}
	if len(stored.RouteRules) != 0 {
		t.Fatalf("corrupt rules should degrade to empty, got %#v", stored.RouteRules)
	}
	// The fallback layer keeps working.
	if fallback := stored.FallbackFor(egress.ScopeBuild); fallback.Mode != egress.FallbackModeNone {
		t.Fatalf("fallback config lost: %#v", fallback)
	}
}

// Sticky per-account proxy templates must be rejected as fixed route-rule
// targets because their exit rotates with the caller identity.
func TestServiceRejectsStickyProxyAsRouteRuleTarget(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	encrypted, err := cipher.Encrypt("http://{account}:pass@proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	sticky, err := nodes.CreateEgressNode(ctx, egress.Node{Name: "sticky-template", Scope: egress.ScopeBuild, Enabled: true, EncryptedProxyURL: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	service := egressapp.NewService(nodes, cipher, "test-browser")

	_, err = service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []egressapp.RouteRuleInput{
			{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: sticky.ID, Enabled: true},
		},
	})
	if err == nil {
		t.Fatal("sticky template node must be rejected as a fixed route-rule target")
	}
}

// Editing a node that serves as a fixed route-rule target into an
// unusable state must be rejected, mirroring the fallback guard.
func TestServiceRejectsRouteRuleTargetDegradation(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "guard-target", 0)
	service := egressapp.NewService(nodes, cipher, "test-browser")

	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []egressapp.RouteRuleInput{
			{Scope: egress.ScopeBuild, Class: egress.TrafficClassInference, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: target.ID, Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Switching scope makes the node incompatible with the build rule.
	if _, err := service.Update(ctx, target.ID, egressapp.Input{Name: "guard-target", Scope: egress.ScopeWeb, Enabled: true}); err == nil {
		t.Fatal("scope change on a rule target must be rejected")
	}
	// Disabling the target must also be rejected.
	if _, err := service.Update(ctx, target.ID, egressapp.Input{Name: "guard-target", Scope: egress.ScopeBuild, Enabled: false}); err == nil {
		t.Fatal("disable of a rule target must be rejected")
	}
	// An unrelated edit stays allowed.
	if _, err := service.Update(ctx, target.ID, egressapp.Input{Name: "guard-target-renamed", Scope: egress.ScopeBuild, Enabled: true}); err != nil {
		t.Fatalf("unrelated edit should pass: %v", err)
	}
}
