package egress

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	physical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"testing"
	"time"

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
			manager := NewManagerWithLimits(repository, cipher, netbudget.Limits{})
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			ctx, _ := physical.WithTrace(context.Background())
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

// 自动调度对池模式端点使用 domain 唯一的"旋转端点无健康惩罚"投影:单个坏
// IP 不参与健康度/亲和排序,也不写回存储;非池模式端点的真实健康保持不变。
// 该投影曾在管理端列表与调度路径各写一份字面量,口径可能再次分叉。
func TestPoolModeSchedulingUsesRotatingEndpointHealthProjection(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	cooldown := time.Now().UTC().Add(time.Minute)
	repository := &mutableEgressRepository{node: egress.Node{
		ID: 1, Name: "pool", Enabled: true, ProxyPool: true,
		Health: 0.2, FailureCount: 4, CooldownUntil: &cooldown, LastError: egress.LastErrorTransport,
	}}
	manager := NewManagerWithLimits(repository, cipher, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	lease, configured, err := manager.AcquireIfConfigured(context.Background(), egress.ScopeBuild, "")
	if err != nil || !configured || lease == nil {
		t.Fatalf("acquire: configured=%v lease=%#v err=%v", configured, lease, err)
	}
	defer lease.Release()

	want := egress.RotatingEndpointHealth(repository.node.HealthState())
	baseline := lease.healthBaseline
	if baseline.Health != want.Health || baseline.FailureCount != want.FailureCount || baseline.CooldownUntil != nil || baseline.LastError != "" {
		t.Fatalf("pool scheduling baseline = %+v, want rotating-endpoint projection %+v", baseline, want)
	}
	if repository.node.Health != 0.2 || repository.node.FailureCount != 4 || repository.node.CooldownUntil == nil || repository.node.LastError != egress.LastErrorTransport {
		t.Fatalf("scheduling projection must not rewrite stored health: %+v", repository.node)
	}
}
