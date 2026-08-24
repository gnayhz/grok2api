package relational

import (
	"context"
	"testing"
	"time"

	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// 订阅同步换代理地址时的观测失效语义:feed 条目换了出口地址,节点旧地址上
// 产生的健康/冷却/失败计数对新地址无效——管理端编辑(applyInput
// configurationChanged)对同一情形整组重置,同步 upsert 却原样保留。
func TestUpsertEgressNodesFromSourceResetsObservationsOnProxyChange(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)

	source, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{Name: "feed", Enabled: true, EncryptedURL: "enc", RefreshIntervalSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	oldProxy, err := cipher.Encrypt("socks5://10.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	// 直接落库一个"已运行且已劣化"的订阅节点(模拟旧地址上积累的观测)。
	cooldown := time.Now().UTC().Add(20 * time.Minute)
	base := egress.Node{
		Name: "feed-0", Enabled: true, SourceID: source.ID, SourceKey: "k1",
		EncryptedProxyURL: oldProxy, Health: 0.05, FailureCount: 7, CooldownUntil: &cooldown,
		LastError: "transport error", ProbeStatus: egress.ProbeStatusUnhealthy, ExitIP: "198.51.100.7",
	}
	if _, err := repo.CreateEgressNode(ctx, base); err != nil {
		t.Fatal(err)
	}

	// 同一 source_key 的新地址重同步。
	newProxy, err := cipher.Encrypt("socks5://10.0.0.2:1080")
	if err != nil {
		t.Fatal(err)
	}
	returned, err := repo.UpsertEgressNodesFromSource(ctx, source.ID, []egress.Node{{
		Name: "feed-0", Enabled: true, SourceID: source.ID, SourceKey: "k1",
		EncryptedProxyURL: newProxy, Health: 1, ProbeStatus: egress.ProbeStatusUnknown,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if returned != 0 {
		t.Fatalf("same-key resync counted as new: %d", returned)
	}

	nodes, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil || len(nodes) != 1 {
		t.Fatalf("list: %v (%d)", err, len(nodes))
	}
	node, err := repo.GetEgressNode(ctx, nodes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if node.EncryptedProxyURL != newProxy {
		t.Fatalf("proxy not updated: %q", node.EncryptedProxyURL)
	}
	// 旧地址的观测必须失效:健康重置、冷却清除、退出 IP 清空。
	if node.Health != 1 || node.FailureCount != 0 || node.CooldownUntil != nil || node.LastError != "" || node.ExitIP != "" {
		t.Fatalf("stale observations from old proxy survived URL change: health=%v fc=%d cooldown=%v lastErr=%q exitIP=%q",
			node.Health, node.FailureCount, node.CooldownUntil, node.LastError, node.ExitIP)
	}
}
