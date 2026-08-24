package cli

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// poolRuleRepository 在固定节点仓储之上叠加专属池仓储面。
type poolRuleRepository struct {
	routeRuleEgressRepository
	pool   map[uint64]domainegress.Pool
	member map[uint64][]domainegress.Node
}

func (r poolRuleRepository) GetEgressPool(_ context.Context, id uint64) (domainegress.Pool, error) {
	if pool, ok := r.pool[id]; ok {
		return pool, nil
	}
	return domainegress.Pool{}, errPoolNotFoundForTest
}

func (r poolRuleRepository) ListEgressNodesByPool(_ context.Context, poolID uint64) ([]domainegress.Node, error) {
	return r.member[poolID], nil
}

// 自动调度只面向未入池节点,与 Manager 的选择规则一致。
func (r poolRuleRepository) ListEgressNodes(context.Context, repository.SortQuery) ([]domainegress.Node, error) {
	var nodes []domainegress.Node
	for _, node := range r.routeRuleEgressRepository.nodes {
		if len(node.PoolIDs) == 0 {
			nodes = append(nodes, node)
		}
	}
	return nodes, nil
}

// 验收链路:inference 类目标(pool) → Build transport → 池内节点代理真实转发。
// 两个代理计数器区分出口:池成员(101)必须收到请求,fallback(直连)不得收到。
func TestPoolTargetRoutesThroughPoolMember(t *testing.T) {
	poolProxyURL, poolCalls, upstream := newRouteRuleProxyPair(t)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	poolProxy, err := cipher.Encrypt(poolProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	poolID := uint64(5)
	member := domainegress.Node{ID: 101, Name: "pool-member", Enabled: true, Health: 1, EncryptedProxyURL: poolProxy, PoolIDs: []uint64{poolID}}
	repo := poolRuleRepository{
		routeRuleEgressRepository: routeRuleEgressRepository{
			nodes: map[uint64]domainegress.Node{101: member},
			config: domainegress.OperationsConfig{ClassTargets: map[domainegress.TrafficClass]domainegress.RoutingTarget{
				domainegress.TrafficClassInference: {Mode: domainegress.RoutingTargetPool, PoolID: poolID},
			}},
		},
		pool:   map[uint64]domainegress.Pool{poolID: {ID: poolID, Name: "premium", Enabled: true}},
		member: map[uint64][]domainegress.Node{poolID: {member}},
	}
	manager := infraegress.NewManager(repo, cipher)
	transport := &egressTransport{manager: manager, fallback: http.DefaultTransport}

	ctx := infraegress.WithTrafficClass(context.Background(), domainegress.TrafficClassInference)
	ctx = infraegress.WithAccountIdentity(ctx, "acct-pool-1")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("roundtrip via pool member failed: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	// 验收核心:请求确实经由池成员代理转发(而非直连/其他出口)。
	if got := atomic.LoadInt64(poolCalls); got != 1 {
		t.Fatalf("pool member proxy calls = %d, want 1", got)
	}
}

// 验收链路:池耗尽(成员全隔离)时逐级退回——不再死守空池,落到自动调度。
func TestPoolTargetExhaustedFallsBackToAutomaticSchedule(t *testing.T) {
	fallbackProxyURL, fallbackCalls, upstream := newRouteRuleProxyPair(t)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	fallbackProxy, err := cipher.Encrypt(fallbackProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	// 池成员在冷却中;未入池节点(201)可供自动调度。
	poolID := uint64(6)
	until := testFutureTime()
	quarantined := domainegress.Node{ID: 101, Name: "cooled", Enabled: true, Health: 1, EncryptedProxyURL: fallbackProxy, CooldownUntil: &until, PoolIDs: []uint64{poolID}}
	scopeNode := domainegress.Node{ID: 201, Name: "auto-exit", Enabled: true, Health: 1, EncryptedProxyURL: fallbackProxy}
	repo := poolRuleRepository{
		routeRuleEgressRepository: routeRuleEgressRepository{
			nodes: map[uint64]domainegress.Node{101: quarantined, 201: scopeNode},
			config: domainegress.OperationsConfig{ClassTargets: map[domainegress.TrafficClass]domainegress.RoutingTarget{
				domainegress.TrafficClassInference: {Mode: domainegress.RoutingTargetPool, PoolID: poolID},
			}},
		},
		pool:   map[uint64]domainegress.Pool{poolID: {ID: poolID, Name: "exhausted", Enabled: true}},
		member: map[uint64][]domainegress.Node{poolID: {quarantined}},
	}
	manager := infraegress.NewManager(repo, cipher)
	transport := &egressTransport{manager: manager, fallback: http.DefaultTransport}

	ctx := infraegress.WithTrafficClass(context.Background(), domainegress.TrafficClassInference)
	ctx = infraegress.WithAccountIdentity(ctx, "acct-pool-2")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("roundtrip after pool exhaustion failed: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if got := atomic.LoadInt64(fallbackCalls); got != 1 {
		t.Fatalf("automatic-schedule fallback proxy calls = %d, want 1", got)
	}
}

var errPoolNotFoundForTest = repository.ErrNotFound

func testFutureTime() time.Time { return time.Now().Add(time.Hour).UTC() }
