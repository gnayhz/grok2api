package egress

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestSelectionPoolMeansExternalPool：Pool 的语义是"外部代理池"——出口 IP
// 由服务商自动更换(粘性/每请求),节点级 ProxyPool 标志即声明,与换 IP
// Webhook 无关。自建 WARP 出口(Webhook/RotationEnabled)是固定 IP,不是池;
// 历史上"固定 IP 误勾代理池"的案例由保存时的互斥校验拒绝,不再由运行时
// 判定兜底(曾要求 ProxyPool && RotationEnabled,导致无 Webhook 的真代理池
// 被误按固定出口惩罚)。
func TestSelectionPoolMeansExternalPool(t *testing.T) {
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
		{name: "external pool node (flag only)", node: egress.Node{ID: 1, Name: "pool", Enabled: true, ProxyPool: true}, wantPool: true},
		{name: "self-hosted warp node (rotation, no pool flag)", node: egress.Node{ID: 3, Name: "warp", Enabled: true, RotationEnabled: true}, wantPool: false},
		{name: "plain fixed proxy", node: egress.Node{ID: 4, Name: "fixed", Enabled: true}, wantPool: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repository := &mutableEgressRepository{node: tc.node}
			manager := NewManager(repository, cipher)
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
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
				t.Fatalf("Pool = %v, want %v (external-pool semantics: ProxyPool flag)", selection.Pool, tc.wantPool)
			}
			if lease.ConnectionPolicy().Fresh != tc.wantPool {
				t.Fatalf("freshTunnel = %v, want %v (same pool predicate as Selection.Pool)", lease.ConnectionPolicy().Fresh, tc.wantPool)
			}
			if lease.proxyPool != tc.wantPool {
				t.Fatalf("proxyPool = %v, want %v (connection retry only on rotating exits)", lease.proxyPool, tc.wantPool)
			}
		})
	}
}
