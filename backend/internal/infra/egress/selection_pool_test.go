package egress

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestSelectionPoolRequiresRotation：Pool 的语义是"旋转出口"（同节点连续请求出口
// IP 不同）。生产实证：proxy_pool=1 但 rotation_enabled=0 的固定 IP 代理
// 被误判为旋转池，节点级降智标记与同号重试等池语义作用在固定脏 IP 上。
func TestSelectionPoolRequiresRotation(t *testing.T) {
	t.Parallel()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		node     egress.Node
		wantPool bool
	}{
		{name: "fixed-ip proxy with pool flag only", node: egress.Node{ID: 1, Name: "fixed", Enabled: true, ProxyPool: true}, wantPool: false},
		{name: "rotating pool node", node: egress.Node{ID: 2, Name: "rot", Enabled: true, ProxyPool: true, RotationEnabled: true}, wantPool: true},
		{name: "rotation without pool flag", node: egress.Node{ID: 3, Name: "odd", Enabled: true, RotationEnabled: true}, wantPool: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repository := &mutableEgressRepository{node: tc.node}
			manager := NewManager(repository, cipher)
			ctx, _ := WithTrace(context.Background())
			lease, configured, err := manager.AcquireIfConfigured(ctx, egress.ScopeBuild, "")
			if err != nil || !configured || lease == nil {
				t.Fatalf("acquire: configured=%v lease=%#v err=%v", configured, lease, err)
			}
			defer lease.Release()
			trace := TraceFromContext(ctx)
			selection, ok := trace.Selection(egress.ScopeBuild)
			if !ok {
				t.Fatal("no selection recorded")
			}
			if selection.Pool != tc.wantPool {
				t.Fatalf("Pool = %v, want %v (rotating semantics: ProxyPool && RotationEnabled)", selection.Pool, tc.wantPool)
			}
			if lease.freshTunnel != tc.wantPool {
				t.Fatalf("freshTunnel = %v, want %v (same rotating predicate as Selection.Pool)", lease.freshTunnel, tc.wantPool)
			}
			if lease.proxyPool != tc.wantPool {
				t.Fatalf("proxyPool = %v, want %v (connection retry only on rotating exits)", lease.proxyPool, tc.wantPool)
			}
		})
	}
}
