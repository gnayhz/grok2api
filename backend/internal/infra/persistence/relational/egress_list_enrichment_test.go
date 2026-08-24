package relational

import (
	"context"
	"fmt"
	"testing"

	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// ListEgressNodes 富集查询的惰性化:无订阅来源时不得查询 sources 表;
// 空节点库不得查询任何富集表;有来源/有节点时投影语义与之前完全一致。
func TestListEgressNodesLazyEnrichment(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewEgressRepository(database)

	// 空库:零查询富集(结果为空切片,无错误)。
	empty, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty list = %d", len(empty))
	}

	// 纯手工节点(无来源):SourceName 必须为空,不因跳过查询而误填。
	cipher := egressOperationsCipher(t)
	encrypted, err := cipher.Encrypt("socks5://127.0.0.1:52883")
	if err != nil {
		t.Fatal(err)
	}
	manual, err := repo.CreateEgressNode(ctx, egress.Node{Name: "manual-only", Enabled: true, EncryptedProxyURL: encrypted, Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	manualList, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(manualList) != 1 || manualList[0].SourceName != "" || manualList[0].SourceID != 0 {
		t.Fatalf("manual node projection = %+v", manualList[0])
	}
	_ = manual

	// 订阅节点:SourceName/PoolIDs 投影必须照常富集。
	source, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{Name: "feed", Enabled: true, EncryptedURL: "enc", RefreshIntervalSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := repo.CreateEgressNode(ctx, egress.Node{Name: "managed", Enabled: true, EncryptedProxyURL: encrypted, Health: 1, SourceID: source.ID, SourceKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := repo.CreateEgressPool(ctx, egress.Pool{Name: "p", Enabled: true, Strategy: egress.PoolStrategyRandom})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{managed.ID}); err != nil {
		t.Fatal(err)
	}
	full, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]egress.Node{}
	for _, node := range full {
		byName[node.Name] = node
	}
	if byName["managed"].SourceName != "feed" {
		t.Fatalf("managed SourceName = %q, want feed", byName["managed"].SourceName)
	}
	if len(byName["managed"].PoolIDs) != 1 || byName["managed"].PoolIDs[0] != pool.ID {
		t.Fatalf("managed PoolIDs = %v, want [%d]", byName["managed"].PoolIDs, pool.ID)
	}
	if byName["manual-only"].SourceName != "" || len(byName["manual-only"].PoolIDs) != 0 {
		t.Fatalf("manual projection polluted: %+v", byName["manual-only"])
	}
}

// GetEgressNode 单节点投影与列表路径一致:同一节点经两路径读出的
// PoolIDs 必须相同(顺序语义:priority 排序)。点查优化(仅查该节点的
// 成员引用)不得改变投影。
func TestGetEgressNodePoolProjectionMatchesList(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	encrypted, err := cipher.Encrypt("socks5://127.0.0.1:52883")
	if err != nil {
		t.Fatal(err)
	}
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "shared", Enabled: true, EncryptedProxyURL: encrypted, Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	var poolIDs []uint64
	for i := 0; i < 3; i++ {
		pool, err := repo.CreateEgressPool(ctx, egress.Pool{Name: fmt.Sprintf("p%d", i), Enabled: true, Strategy: egress.PoolStrategyRandom})
		if err != nil {
			t.Fatal(err)
		}
		poolIDs = append(poolIDs, pool.ID)
		if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{node.ID}); err != nil {
			t.Fatal(err)
		}
	}

	single, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("list = %d nodes", len(listed))
	}
	if !equalUint64s(single.PoolIDs, listed[0].PoolIDs) {
		t.Fatalf("single-read PoolIDs %v != list-read PoolIDs %v", single.PoolIDs, listed[0].PoolIDs)
	}
	if !equalUint64s(single.PoolIDs, poolIDs) {
		t.Fatalf("PoolIDs %v, want creation order %v", single.PoolIDs, poolIDs)
	}
}

func equalUint64s(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
