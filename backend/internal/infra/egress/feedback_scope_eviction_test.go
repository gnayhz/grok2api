package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type evictionScopeRepo struct {
	egressRepositoryTestStub
	node domain.Node
}

func (r *evictionScopeRepo) GetEgressNode(_ context.Context, _ uint64) (domain.Node, error) {
	return r.node, nil
}

func (r *evictionScopeRepo) ListEgressNodes(context.Context, repository.SortQuery) ([]domain.Node, error) {
	return []domain.Node{r.node}, nil
}

// TestFeedback403EvictsOnlyBrowserScopeClients 锁定 403 驱逐粒度:
// 反爬 403 是浏览器作用域的拒绝,只驱逐该 scope 客户端;同节点 Build 通道
// (不经反爬、不携带 clearance cookie)的热连接池不得被连坐清空。
// 传输错误仍按节点整体驱逐(拨号器被所有 scope 共享)。
func TestFeedback403EvictsOnlyBrowserScopeClients(t *testing.T) {
	manager, _ := newPoolTestManager(t)
	manager.repository = &evictionScopeRepo{node: domain.Node{ID: 9, Name: "mixed", Enabled: true, Health: 1}}
	manager.accountIsolated.Store(false)

	buildClient, err := manager.clientForWithOptions(9, domain.ScopeBuild, "http://proxy:8080", "", "", false, "shared", clientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	webClient, err := manager.clientForWithOptions(9, domain.ScopeWeb, "http://proxy:8080", "UA/1.0", "", false, "shared", clientOptions{})
	if err != nil {
		t.Fatal(err)
	}

	manager.FeedbackForScope(context.Background(), domain.ScopeWeb, 9, 403, nil)

	manager.clientMu.Lock()
	var buildAlive, webAlive bool
	for key, cached := range manager.clients {
		if key.nodeID != 9 {
			continue
		}
		switch key.scope {
		case domain.ScopeBuild:
			buildAlive = cached.client == buildClient.client
		case domain.ScopeWeb:
			webAlive = cached.client == webClient.client
		}
	}
	manager.clientMu.Unlock()
	if !buildAlive {
		t.Fatal("Build client was evicted by a browser-scope 403; anti-bot rejection must not collateralize the Build pool")
	}
	if webAlive {
		t.Fatal("Web client survived its own 403 feedback")
	}
}

// TestFeedbackTransportErrorStillEvictsAllScopes 锁定传输错误保留全节点
// 驱逐:拨号器/传输层为同节点所有 scope 共享,链路层故障的驱逐范围不变。
func TestFeedbackTransportErrorStillEvictsAllScopes(t *testing.T) {
	manager, _ := newPoolTestManager(t)
	manager.repository = &evictionScopeRepo{node: domain.Node{ID: 9, Name: "mixed", Enabled: true, Health: 1}}
	manager.accountIsolated.Store(false)

	if _, err := manager.clientForWithOptions(9, domain.ScopeBuild, "http://proxy:8080", "", "", false, "shared", clientOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.clientForWithOptions(9, domain.ScopeWeb, "http://proxy:8080", "UA/1.0", "", false, "shared", clientOptions{}); err != nil {
		t.Fatal(err)
	}

	manager.FeedbackForScope(context.Background(), domain.ScopeWeb, 9, 0, errors.New("connection refused"))

	if managerHasClientForNode(manager, 9) {
		t.Fatal("transport error must evict all clients for the node")
	}
	_ = time.Now
}
