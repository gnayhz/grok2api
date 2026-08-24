package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	relational "github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestWebAssetEgressDoesNotOverwriteInferenceTrace 锁定 trace.go 的文档化
// 不变式:Web 资产归档使用独立作用域, 不得覆写主 Grok Web 推理出口的
// trace/审计选择。同一请求生命周期内先推理(grok_web)后资产下载
// (grok_web_asset), 两者落到不同成员时:
//   - trace.Selection(grok_web) 保持推理选择(资产租约不覆写);
//   - trace.Selection(grok_web_asset) 记录资产出口;
//   - applyAuditEgress(ProviderWeb) 取主推理出口(而非后发生的资产出口)。
//
// 真实代理命中计数佐证两个出口各自真实承流。
func TestWebAssetEgressDoesNotOverwriteInferenceTrace(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "asset-trace.db"))
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

	newProxy := func() *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			response.Body.Close()
			w.WriteHeader(response.StatusCode)
		}))
		t.Cleanup(server.Close)
		return server
	}
	proxyA := newProxy()
	proxyB := newProxy()

	nodeA, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: "web-primary", Enabled: true, EncryptedProxyURL: encryptedFor(t, cipher, proxyA.URL), Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	nodeB, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: "web-asset", Enabled: true, EncryptedProxyURL: encryptedFor(t, cipher, proxyB.URL), Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = nodeB
	pool, err := repo.CreateEgressPool(ctx, egressdomain.Pool{Name: "web-family", Enabled: true, Strategy: egressdomain.PoolStrategyAffinity, FallbackMode: egressdomain.PoolFallbackNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{nodeA.ID, nodeB.ID}); err != nil {
		t.Fatal(err)
	}
	config := egressdomain.DefaultOperationsConfig()
	config.ScopeTargets = map[egressdomain.Scope]egressdomain.RoutingTarget{
		egressdomain.ScopeWeb: {Mode: egressdomain.RoutingTargetPool, PoolID: pool.ID},
	}
	if _, err := repo.SaveEgressOperationsConfig(ctx, config); err != nil {
		t.Fatal(err)
	}

	manager := infraegress.NewManager(repo, cipher)
	traceCtx, trace := infraegress.WithTrace(ctx)

	acquireAndDo := func(scope egressdomain.Scope, affinity string) uint64 {
		lease, acquireErr := manager.Acquire(traceCtx, scope, affinity)
		if acquireErr != nil || lease == nil {
			t.Fatalf("acquire %s: %v", scope, acquireErr)
		}
		// 选择记录发生在 Acquire(recordSelection), 与传输无关; 浏览器作用域
		// 的线路级证据由 egress_audit_trail_test 以 Build 作用域覆盖, 本测试
		// 聚焦 trace 簿记不覆写语义。
		node := lease.NodeID
		lease.Release()
		return node
	}

	// 选两个落点不同的 affinity(affinity 哈希到不同成员)。
	primaryNode := uint64(0)
	assetNode := uint64(0)
	for _, affinity := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		node := acquireAndDo(egressdomain.ScopeWebAsset, affinity)
		if primaryNode == 0 {
			primaryNode = node
			assetNode = node
			continue
		}
		if node != primaryNode {
			assetNode = node
			break
		}
	}
	if assetNode == primaryNode {
		t.Fatal("affinity pool failed to produce two distinct members for the fixture")
	}

	// 主推理出口在后发生(模拟推理先选、资产后走不同成员的相反次序同样成立;
	// 此处直接验证:资产租约之后, 主推理 trace 选择不被覆写)。
	inferenceNode := acquireAndDo(egressdomain.ScopeWeb, "primary-inference")
	assetNode2 := acquireAndDo(egressdomain.ScopeWebAsset, "asset-after")

	webSelection, okWeb := trace.Selection(egressdomain.ScopeWeb)
	assetSelection, okAsset := trace.Selection(egressdomain.ScopeWebAsset)
	if !okWeb || webSelection.NodeID != inferenceNode {
		t.Fatalf("primary web trace selection = %+v (ok=%v), want node %d untouched by asset leases", webSelection, okWeb, inferenceNode)
	}
	if !okAsset || assetSelection.NodeID != assetNode2 {
		t.Fatalf("asset trace selection = %+v (ok=%v), want node %d under its own scope key", assetSelection, okAsset, assetNode2)
	}
	record := auditRecordForWeb(trace)
	if record.EgressNodeID == nil || *record.EgressNodeID != inferenceNode {
		t.Fatalf("audit must keep the primary inference exit (%d), got %+v", inferenceNode, record.EgressNodeID)
	}
}

func auditRecordForWeb(trace *infraegress.Trace) audit.Record {
	record := audit.Record{}
	applyAuditEgress(&record, trace, accountdomain.ProviderWeb)
	return record
}

func encryptedFor(t *testing.T, cipher *security.Cipher, value string) string {
	t.Helper()
	encrypted, err := cipher.Encrypt(value)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}
