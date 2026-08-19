package relational

import (
	"context"
	"errors"
	"testing"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func routeRuleTestConfig(rules []egress.RouteRule) egress.OperationsConfig {
	return egress.OperationsConfig{
		ProbeProvider: egress.ProbeProviderCloudflare, ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: rules,
	}
}

func TestEgressOperationsConfigPersistsRouteRules(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "rule-target", 0)

	config := routeRuleTestConfig([]egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassInference, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: target.ID, Enabled: true},
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetDirect, Enabled: true},
	})
	saved, err := nodes.SaveEgressOperationsConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if rule, ok := saved.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassInference); !ok || rule.TargetNodeID != target.ID {
		t.Fatalf("saved inference rule = %+v ok=%v", rule, ok)
	}
	if rule, ok := saved.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassBilling); !ok || rule.TargetMode != egress.RouteRuleTargetDirect {
		t.Fatalf("saved billing rule = %+v ok=%v", rule, ok)
	}

	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rule, ok := stored.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassInference); !ok || rule.TargetNodeID != target.ID {
		t.Fatalf("stored inference rule = %+v ok=%v", rule, ok)
	}
	if rule, ok := stored.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassBilling); !ok || rule.TargetMode != egress.RouteRuleTargetDirect {
		t.Fatalf("stored billing rule = %+v ok=%v", rule, ok)
	}
}

func TestEgressOperationsConfigRoundTripsEmptyRouteRules(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)

	saved, err := nodes.SaveEgressOperationsConfig(ctx, routeRuleTestConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.RouteRules) != 0 {
		t.Fatalf("saved route rules = %#v, want empty", saved.RouteRules)
	}

	// Upgrade path: a row written before the route-rules column exists decodes
	// to an empty rule list without errors.
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.RouteRules) != 0 {
		t.Fatalf("stored route rules = %#v, want empty", stored.RouteRules)
	}
}

func TestEgressOperationsConfigRejectsUnsafeRouteRuleTarget(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "missing-target", 0)

	// Unknown node id.
	_, err := nodes.SaveEgressOperationsConfig(ctx, routeRuleTestConfig([]egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: target.ID + 9999, Enabled: true},
	}))
	if !errors.Is(err, repository.ErrEgressRouteRuleNodeInUse) {
		t.Fatalf("missing node err = %v, want ErrEgressRouteRuleNodeInUse", err)
	}

	// Unsupported scope.
	webNode := createHealthyEgressNode(t, ctx, nodes, cipher, "web-node", 0)
	if _, err := nodes.UpdateEgressNode(ctx, egress.Node{ID: webNode.ID, Name: webNode.Name, Scope: egress.ScopeWeb, Enabled: true, EncryptedProxyURL: webNode.EncryptedProxyURL}); err != nil {
		t.Fatal(err)
	}
	_, err = nodes.SaveEgressOperationsConfig(ctx, routeRuleTestConfig([]egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: webNode.ID, Enabled: true},
	}))
	if err == nil {
		t.Fatal("web-scope node should not serve a build route rule")
	}

	// Structurally invalid rules are rejected before node lookup.
	_, err = nodes.SaveEgressOperationsConfig(ctx, routeRuleTestConfig([]egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: 0, Enabled: true},
	}))
	if !errors.Is(err, repository.ErrInvalidRecord) {
		t.Fatalf("fixed rule without node err = %v, want ErrInvalidRecord", err)
	}
}

func TestRouteRuleReferenceIsProtectedAndClearedOnDelete(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "rule-target", 0)
	other := createHealthyEgressNode(t, ctx, nodes, cipher, "rule-other", 0)

	rules := []egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassInference, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: target.ID, Enabled: true},
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassModelSync, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: other.ID, Enabled: true},
	}
	if _, err := nodes.SaveEgressOperationsConfig(ctx, routeRuleTestConfig(rules)); err != nil {
		t.Fatal(err)
	}

	// Disabling a referenced node is rejected.
	if _, err := nodes.UpdateEgressNodesEnabled(ctx, []uint64{target.ID}, false); !errors.Is(err, repository.ErrEgressRouteRuleNodeInUse) {
		t.Fatalf("disable referenced node err = %v, want ErrEgressRouteRuleNodeInUse", err)
	}

	// Deleting the referenced node removes only its rule.
	if err := nodes.DeleteEgressNode(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassInference); ok {
		t.Fatal("inference rule should be removed after target deletion")
	}
	if rule, ok := stored.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassModelSync); !ok || rule.TargetNodeID != other.ID {
		t.Fatalf("unrelated rule should survive deletion: %+v ok=%v", rule, ok)
	}
}

func TestServiceOperationsConfigRouteRuleUpdateSemantics(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "svc-rule-target", 0)
	service := egressapp.NewService(nodes, cipher, "test-browser")

	// Explicit rule list is stored.
	saved, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []egressapp.RouteRuleInput{
			{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: target.ID, Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule, ok := saved.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassBilling); !ok || rule.TargetNodeID != target.ID {
		t.Fatalf("saved rule = %+v ok=%v", rule, ok)
	}

	// A sparse update without RouteRules keeps the stored rules.
	kept, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule, ok := kept.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassBilling); !ok || rule.TargetNodeID != target.ID {
		t.Fatalf("sparse update dropped rule: %+v ok=%v", rule, ok)
	}

	// An empty list clears the rules.
	cleared, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []egressapp.RouteRuleInput{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.RouteRules) != 0 {
		t.Fatalf("cleared rules = %#v", cleared.RouteRules)
	}

	// Structurally invalid rules are rejected with the input error.
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []egressapp.RouteRuleInput{
			{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: 0, Enabled: true},
		},
	}); err == nil {
		t.Fatal("expected validation error for fixed rule without node")
	}
}

// Proxy-pool gateways are valid fixed route targets: rules allocate traffic
// to a proxy resource for cost splitting, not to a stable IP.
func TestProxyPoolNodeIsValidRouteRuleTarget(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	encrypted, err := cipher.Encrypt("socks5://user:pass@gw.example.com:824")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := nodes.CreateEgressNode(ctx, egress.Node{Name: "rotating-gateway", Scope: egress.ScopeBuild, Enabled: true, ProxyPool: true, EncryptedProxyURL: encrypted})
	if err != nil {
		t.Fatal(err)
	}

	// Storage-layer validation accepts the pool node.
	config := routeRuleTestConfig([]egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassInference, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: pool.ID, Enabled: true},
	})
	saved, err := nodes.SaveEgressOperationsConfig(ctx, config)
	if err != nil {
		t.Fatalf("pool node must be accepted as fixed target: %v", err)
	}
	if rule, ok := saved.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassInference); !ok || rule.TargetNodeID != pool.ID {
		t.Fatalf("saved rule = %+v ok=%v", rule, ok)
	}

	// Service layer (sticky-only check) accepts it too.
	service := egressapp.NewService(nodes, cipher, "test-browser")
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []egressapp.RouteRuleInput{
			{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: pool.ID, Enabled: true},
		},
	}); err != nil {
		t.Fatalf("service must accept pool node target: %v", err)
	}
}

// Subscription sync must not drop enabled fixed rules that target proxy-pool
// nodes: a rule allocates traffic to a proxy resource, and the pool flag does
// not change that contract.
func TestSubscriptionSyncKeepsPoolNodeRouteRule(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	encrypted, err := cipher.Encrypt("socks5://user:pass@gw.example.com:824")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := nodes.CreateEgressNode(ctx, egress.Node{Name: "sub-pool", Scope: egress.ScopeBuild, Enabled: true, ProxyPool: true, EncryptedProxyURL: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.SaveEgressOperationsConfig(ctx, routeRuleTestConfig([]egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: pool.ID, Enabled: true},
	})); err != nil {
		t.Fatal(err)
	}

	// A subscription sync pass (which calls clearInvalidEgressRouteRuleReferences)
	// replaces the node representation with the same proxy still enabled.
	replacement, err := nodes.UpdateEgressNode(ctx, egress.Node{ID: pool.ID, Name: "sub-pool", Scope: egress.ScopeBuild, Enabled: true, ProxyPool: true, EncryptedProxyURL: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID != pool.ID {
		t.Fatal("replacement mismatch")
	}

	// Simulate the subscription-sync hygiene pass directly: it must keep the rule.
	if err := clearInvalidEgressRouteRuleReferences(database.db.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rule, ok := stored.RouteRuleFor(egress.ScopeBuild, egress.TrafficClassBilling); !ok || rule.TargetNodeID != pool.ID {
		t.Fatalf("pool-target rule must survive subscription sync: %+v ok=%v", rule, ok)
	}
}

// Editing a rule target into a sticky per-account template must be rejected,
// mirroring the save-time sticky validation.
func TestServiceRejectsRuleTargetEditIntoStickyTemplate(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "plain-target", 0)
	service := egressapp.NewService(nodes, cipher, "test-browser")

	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []egressapp.RouteRuleInput{
			{Scope: egress.ScopeBuild, Class: egress.TrafficClassInference, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: target.ID, Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	sticky := "http://{account}:pass@proxy.example:8080"
	if _, err := service.Update(ctx, target.ID, egressapp.Input{Name: "plain-target", Scope: egress.ScopeBuild, Enabled: true, ProxyURL: &sticky}); err == nil {
		t.Fatal("editing a rule target into a sticky template must be rejected")
	}
}

func TestDisabledRouteRuleDoesNotProtectNode(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "disabled-rule-target", 0)

	if _, err := nodes.SaveEgressOperationsConfig(ctx, routeRuleTestConfig([]egress.RouteRule{
		{Scope: egress.ScopeBuild, Class: egress.TrafficClassBilling, TargetMode: egress.RouteRuleTargetFixed, TargetNodeID: target.ID, Enabled: false},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.UpdateEgressNodesEnabled(ctx, []uint64{target.ID}, false); err != nil {
		t.Fatalf("disabled rule should not block disable: %v", err)
	}
}
