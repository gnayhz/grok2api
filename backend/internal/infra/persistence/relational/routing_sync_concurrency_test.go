package relational

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestPostgresRoutingSaveConcurrentWithSync 在真实 PostgreSQL 上核查配置
// 热更新(保存路由固定目标)与订阅同步(换血禁用节点 + 清理悬挂引用)并发
// 时的快照一致性。两条路径都以 SELECT FOR UPDATE 锁定运营配置行串行化;
// 本测试验证:(1) 并发下无死锁/意外错误;(2) 无丢失更新(保存成功后配置
// 必须包含目标, 或被同步的清理显式移除——不会静默变空);(3) 终态不变式:
// 收尾一次同步卫生后, 持久化路由不再引用不可调度的节点。
func TestPostgresRoutingSaveConcurrentWithSync(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)

	encryptedProxy, err := cipher.Encrypt("http://10.99.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "concurrent-target", Enabled: true, EncryptedProxyURL: encryptedProxy, Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	source, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{Name: fmt.Sprintf("concurrent-feed-%d", time.Now().UTC().UnixNano()), Enabled: true, EncryptedURL: "enc", RefreshIntervalSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 25
	var syncErrors, saveErrors, saveAccepted atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// 换血交替:目标节点是手动节点(无 source_key), upsert 不直接触碰
			// 它; 可用性由本侧直接翻转。upsert 事务末尾的悬挂引用清理
			// (clearInvalidEgressRoutingReferences)在每次同步都会执行——
			// 先禁用再同步, 让清理与并发的路由保存真正竞争。
			if i%2 == 1 {
				disabled := node
				disabled.Enabled = false
				if _, err := repo.UpdateEgressNode(ctx, disabled); err != nil {
					syncErrors.Add(1)
					t.Errorf("disable round %d: %v", i, err)
					return
				}
			} else {
				enabled := node
				enabled.Enabled = true
				if _, err := repo.UpdateEgressNode(ctx, enabled); err != nil {
					syncErrors.Add(1)
					t.Errorf("enable round %d: %v", i, err)
					return
				}
			}
			var nodes []egress.Node
			if i%4 == 0 {
				nodes = append(nodes, egress.Node{SourceID: source.ID, SourceKey: "feed-entry", Name: "feed-entry", Enabled: true, EncryptedProxyURL: encryptedProxy, Health: 1})
			}
			if _, err := repo.UpsertEgressNodesFromSource(ctx, source.ID, nodes); err != nil {
				syncErrors.Add(1)
				t.Errorf("sync round %d: %v", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			config := egress.DefaultOperationsConfig()
			config.ScopeTargets = map[egress.Scope]egress.RoutingTarget{
				egress.ScopeBuild: {Mode: egress.RoutingTargetNode, NodeID: node.ID},
			}
			saved, err := repo.SaveEgressOperationsConfig(ctx, config)
			if err != nil {
				// 目标节点恰被禁用时校验拒绝是正确语义, 不计入错误。
				continue
			}
			if saved.ScopeTargets[egress.ScopeBuild].NodeID != node.ID {
				saveErrors.Add(1)
				t.Errorf("save round %d: lost target in accepted save", i)
				return
			}
			saveAccepted.Add(1)
		}
	}()
	wg.Wait()
	if syncErrors.Load() > 0 || saveErrors.Load() > 0 {
		t.Fatalf("unexpected errors: sync=%d save=%d", syncErrors.Load(), saveErrors.Load())
	}

	// 终态不变式:最后一次同步卫生后, 持久化路由不引用不可调度节点。
	final := node
	final.Enabled = false
	if _, err := repo.UpdateEgressNode(ctx, final); err != nil {
		t.Fatal(err)
	}
	// 一次同步即可触发事务内悬挂引用清理。
	if _, err := repo.UpsertEgressNodesFromSource(ctx, source.ID, nil); err != nil {
		t.Fatal(err)
	}
	config, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if target, ok := config.ScopeTargets[egress.ScopeBuild]; ok && target.NodeID == node.ID {
		t.Fatalf("dangling routing target persisted after hygiene: %+v", target)
	}
	if saveAccepted.Load() == 0 {
		t.Fatal("no save was ever accepted - concurrency harness failed to exercise the race")
	}
}
