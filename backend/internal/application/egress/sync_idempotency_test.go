package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	relational "github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// TestSubscriptionSyncIdempotencyUnderFaultInjection 在真实 SQLite + 真实 HTTP
// feed 服务上锁定订阅同步的幂等与故障隔离语义:
//  1. 首次同步导入 N 个节点;
//  2. 相同 feed 重复同步 imported=0、节点总数不变(按 source_key 去重);
//  3. feed 换血: 保留项原位更新, 消失项被停用(不删除、探活状态复位、
//     probe_error 标记 subscription entry removed), 新项导入;
//  4. feed 故障(500): 同步失败但既有节点逐字段不被触碰, 仅记录失败状态;
//  5. feed 恢复并带回曾消失的条目: 该条目重新启用。
func TestSubscriptionSyncIdempotencyUnderFaultInjection(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sync-idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewEgressRepository(database)
	cipher := newRotationCipher(t)

	// 订阅地址必须是公网(SSRF 守卫); 本地 feed 服务充当取料代理——按绝对
	// URI 收到对公网地址的 GET 时直接回 feed 内容, 与 subscription_test 的
	// 夹具同型。
	var mu sync.Mutex
	body := "http://10.0.0.1:1111\nhttp://10.0.0.2:2222\nhttp://10.0.0.3:3333\n"
	status := http.StatusOK
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Host != "1.1.1.1" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(feed.Close)

	encryptedURL, err := cipher.Encrypt("http://1.1.1.1/feed")
	if err != nil {
		t.Fatal(err)
	}
	encryptedFetchProxy, err := cipher.Encrypt(feed.URL)
	if err != nil {
		t.Fatal(err)
	}
	source, err := repo.CreateEgressSource(ctx, domain.SubscriptionSource{Name: "idempotency-feed", Enabled: true, EncryptedURL: encryptedURL, EncryptedProxyURL: encryptedFetchProxy, RefreshIntervalSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}

	service := NewService(repo, cipher)
	countNodes := func() (total, enabled int) {
		nodes, listErr := repo.ListEgressNodes(ctx, repository.SortQuery{})
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, node := range nodes {
			if node.SourceID == source.ID {
				total++
				if node.Enabled {
					enabled++
				}
			}
		}
		return total, enabled
	}

	// 1. 首次同步: 3 个节点导入。
	result, err := service.SyncSource(ctx, source.ID)
	if err != nil || result.Imported != 3 {
		t.Fatalf("first sync = %+v err=%v, want imported=3", result, err)
	}
	if total, enabled := countNodes(); total != 3 || enabled != 3 {
		t.Fatalf("after first sync total=%d enabled=%d, want 3/3", total, enabled)
	}

	// 2. 相同 feed 重复同步: 幂等。
	result, err = service.SyncSource(ctx, source.ID)
	if err != nil || result.Imported != 0 {
		t.Fatalf("repeat sync = %+v err=%v, want imported=0 (dedup by source_key)", result, err)
	}
	if total, enabled := countNodes(); total != 3 || enabled != 3 {
		t.Fatalf("after repeat sync total=%d enabled=%d, want 3/3 (no duplicates)", total, enabled)
	}

	// 3. feed 换血: 移除 :2222, 新增 :4444。
	mu.Lock()
	body = "http://10.0.0.1:1111\nhttp://10.0.0.3:3333\nhttp://10.0.0.4:4444\n"
	mu.Unlock()
	result, err = service.SyncSource(ctx, source.ID)
	if err != nil || result.Imported != 1 {
		t.Fatalf("churn sync = %+v err=%v, want imported=1 (only the new entry)", result, err)
	}
	if total, enabled := countNodes(); total != 4 || enabled != 3 {
		t.Fatalf("after churn total=%d enabled=%d, want 4 total / 3 enabled (removed entry disabled, not deleted)", total, enabled)
	}
	nodes, _ := repo.ListEgressNodes(ctx, repository.SortQuery{})
	for _, node := range nodes {
		if node.SourceID != source.ID {
			continue
		}
		proxyURL, decryptErr := cipher.Decrypt(node.EncryptedProxyURL)
		if decryptErr != nil {
			t.Fatal(decryptErr)
		}
		if proxyURL == "http://10.0.0.2:2222" {
			if node.Enabled {
				t.Fatal("removed feed entry must be disabled")
			}
			if node.ProbeError != "subscription entry removed" {
				t.Fatalf("removed entry probe_error = %q", node.ProbeError)
			}
			if node.ProbeStatus.IsValid() && node.ProbeStatus != "unknown" {
				t.Fatalf("removed entry probe status must reset to unknown, got %q", node.ProbeStatus)
			}
		}
	}

	// 4. feed 故障(500): 同步失败, 既有节点逐字段不动。
	mu.Lock()
	status = http.StatusInternalServerError
	mu.Unlock()
	if _, err := service.SyncSource(ctx, source.ID); err == nil {
		t.Fatal("sync against a failing feed must fail")
	}
	if total, enabled := countNodes(); total != 4 || enabled != 3 {
		t.Fatalf("failed sync mutated nodes: total=%d enabled=%d, want 4/3 untouched", total, enabled)
	}
	failed, err := repo.GetEgressSource(ctx, source.ID)
	if err != nil || failed.LastSyncError == "" {
		t.Fatalf("failure must be recorded on the source: %+v err=%v", failed, err)
	}

	// 5. feed 恢复并带回 :2222: 该条目重新启用。
	mu.Lock()
	status = http.StatusOK
	body = "http://10.0.0.1:1111\nhttp://10.0.0.2:2222\nhttp://10.0.0.3:3333\nhttp://10.0.0.4:4444\n"
	mu.Unlock()
	result, err = service.SyncSource(ctx, source.ID)
	if err != nil || result.Imported != 0 {
		t.Fatalf("recovery sync = %+v err=%v, want imported=0 (all keys known)", result, err)
	}
	if total, enabled := countNodes(); total != 4 || enabled != 4 {
		t.Fatalf("after recovery total=%d enabled=%d, want 4/4 (reappeared entry re-enabled)", total, enabled)
	}
	recovered, err := repo.GetEgressSource(ctx, source.ID)
	if err != nil || recovered.LastSyncError != "" {
		t.Fatalf("successful sync must clear the failure marker: %+v err=%v", recovered, err)
	}
}

// 批量导入文本的输入边界矩阵:空内容/全无效行/超长内容/重复行去重。
// 语义来自 parseProxySubscription + CreateEgressNodes(分批 100):
//   - 空内容 → ErrInvalidInput;
//   - 全无效行 → ErrInvalidInput(无可用条目), 不落库;
//   - 有效+无效混合 → 无效行计入 Skipped, 有效行全部入库;
//   - 重复行 → 解析层按规范化 URL 去重(重复计入 Skipped), 库内不产生重复;
//   - 超过 maxSubscriptionEntries → 解析层拒绝, 不截断静默入库。
func TestImportTextInputBoundaries(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "import-boundary.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewEgressRepository(database)
	service := NewService(repo, newRotationCipher(t))

	countNodes := func() int {
		nodes, listErr := repo.ListEgressNodes(ctx, repository.SortQuery{})
		if listErr != nil {
			t.Fatal(listErr)
		}
		return len(nodes)
	}

	if _, err := service.ImportText(ctx, ImportInput{Name: "empty", Content: "   \n\t "}); err == nil {
		t.Fatal("blank content must be rejected")
	}
	if _, err := service.ImportText(ctx, ImportInput{Name: "all-invalid", Content: "not a proxy\nalso not\n###\n"}); err == nil {
		t.Fatal("all-invalid content must be rejected")
	}
	if countNodes() != 0 {
		t.Fatalf("rejected imports must not create nodes: %d", countNodes())
	}

	result, err := service.ImportText(ctx, ImportInput{Name: "mixed", Content: "http://10.1.0.1:1111\nnot a proxy\nhttp://10.1.0.2:2222\nhttp://10.1.0.1:1111\n"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 || result.Skipped != 2 {
		t.Fatalf("mixed import = %+v, want imported=2 skipped=2 (one invalid line + one duplicate)", result)
	}
	if countNodes() != 2 {
		t.Fatalf("duplicate line must not create a second node: %d", countNodes())
	}

	// 条目数上限:构造 maxSubscriptionEntries+1 个不同代理, 解析层整体拒绝。
	var huge strings.Builder
	for i := 0; i <= 10000; i++ {
		fmt.Fprintf(&huge, "http://10.2.%d.%d:1080\n", i/250, i%250+1)
	}
	if _, err := service.ImportText(ctx, ImportInput{Name: "oversized", Content: huge.String()}); err == nil {
		t.Fatal("entry-count overflow must be rejected, not silently truncated")
	}

	// 字节上限语义边界:2MiB 上限作用于订阅拉取(fetch 层 LimitReader)与
	// base64 解码, 不作用于管理端导入的原始文本——后者由请求体上限
	// (server.maxBodyBytes)与条目数上限约束。注释占多数的 2MB+ 文本但条目
	// 很少时应被接受(仅 1 条有效)。
	big, err := service.ImportText(ctx, ImportInput{Name: "comment-heavy", Content: "http://10.3.0.1:1\n" + strings.Repeat("#"+strings.Repeat("x", 220)+"\n", 10000)})
	if err != nil || big.Imported != 1 {
		t.Fatalf("comment-heavy large import = %+v err=%v, want imported=1 (entry cap governs, not raw bytes)", big, err)
	}
}

// RunMaintenance 编排函数(到期源同步 + 到期节点探测, errors.Join 聚合)此前
// 跨全部测试包零覆盖——组件各自有测试但编排层无。真实 SQLite + 真实 feed:
// 一次 RunMaintenance 应同时驱动到期源的同步与到期节点的探测; 源失败不
// 阻断节点探测(错误聚合), 反之亦然。
func TestRunMaintenanceOrchestratesSyncAndProbe(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "maintenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewEgressRepository(database)
	cipher := newRotationCipher(t)

	// 到期源:指向不可达地址 → 同步失败(验证错误不中断探测)。
	unreachable, err := cipher.Encrypt("http://192.0.2.1:1/feed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateEgressSource(ctx, domain.SubscriptionSource{Name: "due-failing", Enabled: true, EncryptedURL: unreachable, RefreshIntervalSeconds: 60}); err != nil {
		t.Fatal(err)
	}

	// 到期节点:代理指向本地死端口 → 探测不健康(观察字段写入)。
	encryptedProxy, err := cipher.Encrypt("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateEgressNode(ctx, domain.Node{Name: "due-node", Enabled: true, EncryptedProxyURL: encryptedProxy, Health: 1}); err != nil {
		t.Fatal(err)
	}

	service := NewService(repo, cipher)
	probeCalls := 0
	service.SetNodeProber(&maintenanceProber{calls: &probeCalls, result: domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.1"}})

	maintErr := service.RunMaintenance(ctx)
	// 源失败必须被聚合上报(不静默吞掉), 但探测照常执行。
	if maintErr == nil {
		t.Fatal("failing source sync should surface an aggregated error")
	}

	node, err := repo.GetEgressNode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if node.ProbeStatus != domain.ProbeStatusHealthy || node.ExitIP != "198.51.100.1" {
		t.Fatalf("due node was not probed despite source failure: %+v", node)
	}
	if probeCalls == 0 {
		t.Fatal("prober never invoked")
	}

	// 全部成功时:错误为 nil, 源的同步状态被更新。(订阅地址必须公网——
	// SSRF 守卫拒绝环回; 本地 feed 充当取料代理, 同 sync_idempotency 夹具。)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "1.1.1.1" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("http://10.5.0.1:1111\n"))
	}))
	t.Cleanup(feed.Close)
	encryptedFeed, err := cipher.Encrypt("http://1.1.1.1/feed")
	if err != nil {
		t.Fatal(err)
	}
	encryptedFetchProxy, err := cipher.Encrypt(feed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateEgressSource(ctx, domain.SubscriptionSource{Name: "due-healthy", Enabled: true, EncryptedURL: encryptedFeed, EncryptedProxyURL: encryptedFetchProxy, RefreshIntervalSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	// 使 failing 源不再到期:只跑健康源。
	stale, err := repo.GetEgressSource(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := repo.UpdateEgressSourceSync(ctx, stale.ID, time.Now().UTC(), future, 0, ""); err != nil {
		t.Fatal(err)
	}
	// 到期 healthy 源 + 未到期节点(刚探测过)。
	if err := service.RunMaintenance(ctx); err != nil {
		t.Fatalf("all-healthy maintenance should succeed: %v", err)
	}
	synced, err := repo.GetEgressSource(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if synced.LastSyncError != "" || synced.LastSyncImported != 1 {
		t.Fatalf("healthy source not synced: %+v", synced)
	}
}

type maintenanceProber struct {
	calls  *int
	result domain.ProbeResult
}

func (p *maintenanceProber) ProbeEgressNode(context.Context, domain.Node) (domain.ProbeResult, error) {
	*p.calls++
	return p.result, nil
}
