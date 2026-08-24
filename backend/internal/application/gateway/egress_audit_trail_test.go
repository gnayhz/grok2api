package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	relational "github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// TestEgressAuditTrailMatchesActualExit 端到端验证可追踪性验收项:审计记录
// 的出口字段必须与"真实承流的那条出口"一致。链路与生产完全同型:
// WithTrace → 真实 manager Acquire(真实代理) → recordSelection →
// applyAuditEgress。由代理命中计数器证明"真实承流节点", 再逐字段比对
// 审计记录。三种形态:
//  1. 类别路由 → 池成员(经真实代理) → Mode=proxy + 该节点 ID/名称;
//  2. 自动调度直连 → Mode=direct + NodeName="direct";
//  3. 池耗尽回退 direct(allowDirect) → Mode=direct + NodeName="pool-direct"。
func TestEgressAuditTrailMatchesActualExit(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-trail.db"))
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
	repo := relational.NewEgressRepository(database)

	var originHits, proxyAHits, proxyBHits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(origin.Close)
	newProxy := func(hits *atomic.Int64) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			forward, forwardErr := http.NewRequestWithContext(r.Context(), r.Method, r.RequestURI, r.Body)
			if forwardErr != nil {
				http.Error(w, "bad target", http.StatusBadRequest)
				return
			}
			forward.Header = r.Header.Clone()
			response, doErr := http.DefaultTransport.RoundTrip(forward)
			if doErr != nil {
				http.Error(w, "forward failed", http.StatusBadGateway)
				return
			}
			defer response.Body.Close()
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
			_ = response
		}))
		t.Cleanup(server.Close)
		return server
	}
	proxyA := newProxy(&proxyAHits)
	proxyB := newProxy(&proxyBHits)
	encryptedA, _ := cipher.Encrypt(proxyA.URL)
	encryptedB, _ := cipher.Encrypt(proxyB.URL)
	nodeA, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: "audit-warp-a", Enabled: true, EncryptedProxyURL: encryptedA, Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: "audit-warp-b", Enabled: true, EncryptedProxyURL: encryptedB, Health: 1}); err != nil {
		t.Fatal(err)
	}
	pool, err := repo.CreateEgressPool(ctx, egressdomain.Pool{Name: "audit-pool", Enabled: true, Strategy: egressdomain.PoolStrategyAffinity, FallbackMode: egressdomain.PoolFallbackDirect})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{nodeA.ID}); err != nil {
		t.Fatal(err)
	}
	config := egressdomain.DefaultOperationsConfig()
	config.ClassTargets = map[egressdomain.TrafficClass]egressdomain.RoutingTarget{
		egressdomain.TrafficClassInference: {Mode: egressdomain.RoutingTargetPool, PoolID: pool.ID},
	}
	saved, err := repo.SaveEgressOperationsConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	_ = saved

	manager := infraegress.NewManager(repo, cipher)
	roundTrip := func(t *testing.T, traceCtx context.Context) (*audit.Record, uint64) {
		t.Helper()
		trace := infraegress.TraceFromContext(traceCtx)
		lease, acquireErr := manager.Acquire(traceCtx, egressdomain.ScopeBuild, "audit-trail")
		if acquireErr != nil || lease == nil {
			t.Fatalf("acquire: lease=%v err=%v", lease, acquireErr)
		}
		request, requestErr := http.NewRequestWithContext(traceCtx, http.MethodGet, origin.URL+"/audit", nil)
		if requestErr != nil {
			lease.Release()
			t.Fatal(requestErr)
		}
		response, doErr := lease.Do(request)
		if doErr != nil {
			lease.Release()
			t.Fatalf("real round trip: %v", doErr)
		}
		response.Body.Close()
		servedNode := lease.NodeID
		lease.Release()
		record := audit.Record{}
		applyAuditEgress(&record, trace, accountdomain.ProviderBuild)
		return &record, servedNode
	}

	t.Run("pool member via real proxy", func(t *testing.T) {
		traceCtx, _ := infraegress.WithTrace(ctx)
		record, served := roundTrip(t, infraegress.WithTrafficClass(traceCtx, egressdomain.TrafficClassInference))
		if proxyAHits.Load() != 1 || originHits.Load() != 1 {
			t.Fatalf("traffic did not traverse the node's proxy: proxyHits=%d originHits=%d", proxyAHits.Load(), originHits.Load())
		}
		if record.EgressMode != audit.EgressModeProxy || record.EgressNodeID == nil || *record.EgressNodeID != served || record.EgressNodeName != "audit-warp-a" || record.EgressScope != string(egressdomain.ScopeBuild) {
			t.Fatalf("audit trail does not match the actual exit: served=%d record=%+v", served, record)
		}
	})

	t.Run("automatic direct", func(t *testing.T) {
		// 自动调度的直连分支:清空路由并停用全部节点后, Acquire 落到 direct。
		if err := repo.SetEgressPoolMembers(ctx, pool.ID, nil); err != nil {
			t.Fatal(err)
		}
		emptyConfig := egressdomain.DefaultOperationsConfig()
		if _, err := repo.SaveEgressOperationsConfig(ctx, emptyConfig); err != nil {
			t.Fatal(err)
		}
		nodes, listErr := repo.ListEgressNodes(ctx, repository.SortQuery{})
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, node := range nodes {
			node.Enabled = false
			if _, updateErr := repo.UpdateEgressNode(ctx, node); updateErr != nil {
				t.Fatal(updateErr)
			}
		}
		manager.InvalidateOperationsConfig()
		manager.InvalidatePoolCache()
		traceCtx, trace := infraegress.WithTrace(ctx)
		lease, acquireErr := manager.Acquire(traceCtx, egressdomain.ScopeBuild, "audit-direct")
		if acquireErr != nil || lease == nil {
			t.Fatalf("direct acquire: lease=%v err=%v", lease, acquireErr)
		}
		lease.Release()
		record := audit.Record{}
		applyAuditEgress(&record, trace, accountdomain.ProviderBuild)
		if record.EgressMode != audit.EgressModeDirect || record.EgressNodeName != "direct" || record.EgressNodeID != nil {
			t.Fatalf("automatic direct audit mismatch: %+v", record)
		}
	})

	t.Run("pool exhausted falls back direct with marker", func(t *testing.T) {
		// 恢复类别路由 → 池, 但把唯一成员置于冷却: 池 fallback=direct 且
		// 调用方 allowDirect=true 时, 审计应记录 pool-direct 标记。
		if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{nodeA.ID}); err != nil {
			t.Fatal(err)
		}
		poolConfig := egressdomain.DefaultOperationsConfig()
		poolConfig.ClassTargets = map[egressdomain.TrafficClass]egressdomain.RoutingTarget{
			egressdomain.TrafficClassInference: {Mode: egressdomain.RoutingTargetPool, PoolID: pool.ID},
		}
		if _, err := repo.SaveEgressOperationsConfig(ctx, poolConfig); err != nil {
			t.Fatal(err)
		}
		until := time.Now().UTC().Add(time.Hour)
		if err := repo.UpdateEgressNodeHealth(context.Background(), nodeA.ID, 0.5, 2, &until, egressdomain.LastErrorExitIPQuality); err != nil {
			t.Fatal(err)
		}
		manager.InvalidateOperationsConfig()
		manager.InvalidatePoolCache()

		traceCtx, trace := infraegress.WithTrace(ctx)
		lease, outcome, acquireErr := manager.AcquirePoolRouted(infraegress.WithTrafficClass(traceCtx, egressdomain.TrafficClassInference), egressdomain.ScopeBuild, "audit-fallback", pool.ID, true, "")
		if acquireErr != nil || outcome != infraegress.PoolRouteDirect || lease == nil {
			t.Fatalf("pool-direct fallback: lease=%v outcome=%v err=%v", lease, outcome, acquireErr)
		}
		lease.Release()
		record := audit.Record{}
		applyAuditEgress(&record, trace, accountdomain.ProviderBuild)
		if record.EgressMode != audit.EgressModeDirect || record.EgressNodeName != "pool-direct" || record.EgressNodeID != nil {
			t.Fatalf("pool-direct audit mismatch: %+v", record)
		}
	})
}
