package egress

import (
	"context"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// routingHygieneStub covers the repositories the hygiene pass touches.
type routingHygieneStub struct {
	ServiceRepository
	OperationsRepository
	config    domain.OperationsConfig
	node      domain.Node
	nodeErr   error
	saved     []domain.OperationsConfig
	saveCalls int
}

func (r *routingHygieneStub) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	return r.config, nil
}

func (r *routingHygieneStub) SaveEgressOperationsConfig(_ context.Context, config domain.OperationsConfig) (domain.OperationsConfig, error) {
	r.saveCalls++
	r.saved = append(r.saved, config)
	return config, nil
}

func (r *routingHygieneStub) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	return r.node, r.nodeErr
}

// TestSyncHygieneStripsTargetWhenNodeBecomesAccountTemplate locks the review
// P1 fix: a subscription refresh can rewrite a fixed-target node into an
// {account} template while bypassing the node-edit guard. The repository
// hygiene cannot decrypt, so the application pass must strip the target.
func TestSyncHygieneStripsTargetWhenNodeBecomesAccountTemplate(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("socks5h://Default.{account}:token@resin.example:2260")
	if err != nil {
		t.Fatal(err)
	}
	stub := &routingHygieneStub{
		config: domain.OperationsConfig{
			DefaultTarget: domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: 7},
			ScopeTargets: map[domain.Scope]domain.RoutingTarget{
				domain.ScopeBuild: {Mode: domain.RoutingTargetNode, NodeID: 7},
			},
			ClassTargets: map[domain.TrafficClass]domain.RoutingTarget{
				domain.TrafficClassInference: {Mode: domain.RoutingTargetDirect},
			},
		},
		node: domain.Node{ID: 7, Enabled: true, EncryptedProxyURL: encrypted},
	}
	service := &Service{cipher: cipher, repository: stub}
	if err := service.enforceRoutingHygieneAfterSync(context.Background(), stub); err != nil {
		t.Fatal(err)
	}
	if stub.saveCalls != 1 || len(stub.saved) != 1 {
		t.Fatalf("hygiene must persist the filtered config: saveCalls=%d", stub.saveCalls)
	}
	saved := stub.saved[0]
	if saved.DefaultTarget.Configured() {
		t.Errorf("template default target must be stripped: %+v", saved.DefaultTarget)
	}
	if _, ok := saved.ScopeTargets[domain.ScopeBuild]; ok {
		t.Errorf("template scope target must be stripped: %+v", saved.ScopeTargets)
	}
	// Non-node targets pass through untouched.
	if got, ok := saved.ClassTargets[domain.TrafficClassInference]; !ok || got.Mode != domain.RoutingTargetDirect {
		t.Errorf("direct class target must survive: %+v", saved.ClassTargets)
	}
}

// TestSyncHygieneKeepsHealthyFixedTarget proves valid node targets are kept.
func TestSyncHygieneKeepsHealthyFixedTarget(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("socks5h://stable.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	target := domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: 7}
	stub := &routingHygieneStub{
		config: domain.OperationsConfig{
			DefaultTarget: target,
			ScopeTargets: map[domain.Scope]domain.RoutingTarget{
				domain.ScopeBuild: target,
			},
		},
		node: domain.Node{ID: 7, Enabled: true, EncryptedProxyURL: encrypted},
	}
	service := &Service{cipher: cipher, repository: stub}
	if err := service.enforceRoutingHygieneAfterSync(context.Background(), stub); err != nil {
		t.Fatal(err)
	}
	// 无需剥离即不写:健康配置的整行写回只会打开丢失更新窗口并放大写入,
	// 不再是卫生检查的副作用。存活性由下面剥离场景的直写断言覆盖。
	if stub.saveCalls != 0 {
		t.Fatalf("healthy config must not be rewritten by hygiene: saveCalls=%d", stub.saveCalls)
	}
	if stub.config.DefaultTarget.NodeID != 7 {
		t.Errorf("healthy default target must survive untouched: %+v", stub.config.DefaultTarget)
	}
	if got := stub.config.ScopeTargets[domain.ScopeBuild]; got.NodeID != 7 {
		t.Errorf("healthy scope target must survive untouched: %+v", got)
	}
}

// TestSyncHygieneStripsUnschedulableNode targets that CanNodeServeFixedTarget
// rejects (disabled, proxy-pool) must also be stripped, not just templates.
func TestSyncHygieneStripsUnschedulableNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	stub := &routingHygieneStub{
		config: domain.OperationsConfig{
			DefaultTarget: domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: 8},
		},
		node: domain.Node{ID: 8, Enabled: false, EncryptedProxyURL: "whatever"},
	}
	service := &Service{cipher: cipher, repository: stub}
	if err := service.enforceRoutingHygieneAfterSync(context.Background(), stub); err != nil {
		t.Fatal(err)
	}
	if stub.saved[0].DefaultTarget.Configured() {
		t.Errorf("disabled node target must be stripped: %+v", stub.saved[0].DefaultTarget)
	}
}
