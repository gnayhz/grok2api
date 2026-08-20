package egress

import (
	"context"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// routeRuleHygieneStub covers the repositories the hygiene pass touches.
type routeRuleHygieneStub struct {
	ServiceRepository
	OperationsRepository
	config    domain.OperationsConfig
	node      domain.Node
	nodeErr   error
	saved     []domain.OperationsConfig
	saveCalls int
}

func (r *routeRuleHygieneStub) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	return r.config, nil
}

func (r *routeRuleHygieneStub) SaveEgressOperationsConfig(_ context.Context, config domain.OperationsConfig) (domain.OperationsConfig, error) {
	r.saveCalls++
	r.saved = append(r.saved, config)
	return config, nil
}

func (r *routeRuleHygieneStub) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	return r.node, r.nodeErr
}

// TestSyncHygieneStripsRuleWhenTargetBecomesAccountTemplate locks the review
// P1 fix: a subscription refresh can rewrite a rule fixed-target node into
// an {account} template while bypassing the node-edit guard. The repository
// hygiene cannot decrypt, so the application pass must strip the rule.
func TestSyncHygieneStripsRuleWhenTargetBecomesAccountTemplate(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("socks5h://Default.{account}:token@resin.example:2260")
	if err != nil {
		t.Fatal(err)
	}
	stub := &routeRuleHygieneStub{
		config: domain.OperationsConfig{RouteRules: []domain.RouteRule{{
			Scope: domain.ScopeBuild, Class: domain.TrafficClassInference,
			Enabled: true, TargetMode: domain.RouteRuleTargetFixed, TargetNodeID: 7,
		}}},
		node: domain.Node{ID: 7, Scope: domain.ScopeBuild, Enabled: true, EncryptedProxyURL: encrypted},
	}
	service := &Service{cipher: cipher, repository: stub}
	if err := service.enforceRouteRuleHygieneAfterSync(context.Background(), stub); err != nil {
		t.Fatal(err)
	}
	if stub.saveCalls != 1 || len(stub.saved) != 1 || len(stub.saved[0].RouteRules) != 0 {
		t.Fatalf("template target must be stripped: saveCalls=%d rules=%#v", stub.saveCalls, stub.saved)
	}
}

// TestSyncHygieneKeepsHealthyFixedTarget proves valid rules are untouched.
func TestSyncHygieneKeepsHealthyFixedTarget(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("socks5h://stable.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	stub := &routeRuleHygieneStub{
		config: domain.OperationsConfig{RouteRules: []domain.RouteRule{{
			Scope: domain.ScopeBuild, Class: domain.TrafficClassInference,
			Enabled: true, TargetMode: domain.RouteRuleTargetFixed, TargetNodeID: 7,
		}}},
		node: domain.Node{ID: 7, Scope: domain.ScopeBuild, Enabled: true, EncryptedProxyURL: encrypted},
	}
	service := &Service{cipher: cipher, repository: stub}
	if err := service.enforceRouteRuleHygieneAfterSync(context.Background(), stub); err != nil {
		t.Fatal(err)
	}
	if stub.saveCalls != 0 {
		t.Fatalf("healthy target must not trigger a save: saveCalls=%d", stub.saveCalls)
	}
}
