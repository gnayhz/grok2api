package egress

import (
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// nodePoolRefs 投影三分支:无成员→nil(不产生空数组)、名称表缺失→裸 ID 引用
// (名称解析失败不得连坐吞掉 ID)、名称表命中→{id,name} 完整引用。
// 经真实服务路径驱动(建池→设成员→列表),而非直调私有函数。
func TestNodePoolRefsProjectionServicePath(t *testing.T) {
	ctx, service, repo := newPoolServiceFixture(t)

	cipher := newRotationCipher(t)
	encrypted, err := cipher.Encrypt("socks5://127.0.0.1:52883")
	if err != nil {
		t.Fatal(err)
	}
	member, err := repo.CreateEgressNode(ctx, domain.Node{Name: "pooled", Enabled: true, EncryptedProxyURL: encrypted, Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateEgressNode(ctx, domain.Node{Name: "loner", Enabled: true, EncryptedProxyURL: encrypted, Health: 1}); err != nil {
		t.Fatal(err)
	}

	poolA, err := service.CreatePool(ctx, PoolInput{Name: "alpha-pool", Strategy: "random", FallbackMode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetPoolMembers(ctx, poolA.ID, []uint64{member.ID}); err != nil {
		t.Fatal(err)
	}

	nodes, _, err := service.List(ctx, 1, 10, "", ListFilter{Sort: repository.SortQuery{Field: "name", Direction: repository.SortAscending}})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]domain.PublicNode{}
	for _, node := range nodes {
		byName[node.Name] = node
	}

	// 成员节点:完整 {id,name} 引用。
	pooled := byName["pooled"]
	if len(pooled.Pools) != 1 || pooled.Pools[0].ID != poolA.ID || pooled.Pools[0].Name != "alpha-pool" {
		t.Fatalf("pooled node refs = %+v, want [{%d alpha-pool}]", pooled.Pools, poolA.ID)
	}
	// 非成员节点:Pools 为 nil(零值切片),不是空数组。
	if byName["loner"].Pools != nil {
		t.Fatalf("loner Pools = %+v, want nil", byName["loner"].Pools)
	}
}
