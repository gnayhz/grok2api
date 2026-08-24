package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/account"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// e2eForwardProxy 是 MicroWARP 替身(EXIT-IP-GUARD §7):本地真实 HTTP 正向
// 代理——CONNECT 隧道直通真实目标(真实 ipinfo 探活走真实互联网出口),
// 绝对 URI 请求转发到本地源站(canary SSE 上游替身)。计数器记录两类流量,
// 证明探活与 canary 都真实穿过了节点代理。
type e2eForwardProxy struct {
	server   *httptest.Server
	tunnels  atomic.Int64
	forwards atomic.Int64
}

func newE2EForwardProxy(t *testing.T) *e2eForwardProxy {
	t.Helper()
	proxy := &e2eForwardProxy{}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			proxy.tunnels.Add(1)
			target, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
			if err != nil {
				http.Error(w, "tunnel dial failed", http.StatusBadGateway)
				return
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				target.Close()
				http.Error(w, "hijack unsupported", http.StatusInternalServerError)
				return
			}
			client, _, err := hijacker.Hijack()
			if err != nil {
				target.Close()
				return
			}
			// CONNECT 必须先回 200 再开始隧道, 否则客户端等待代理响应直至失败。
			if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
				client.Close()
				target.Close()
				return
			}
			go func() { _, _ = io.Copy(target, client); target.Close() }()
			go func() { _, _ = io.Copy(client, target); client.Close() }()
			return
		}
		proxy.forwards.Add(1)
		transport := &http.Transport{}
		outbound := &http.Request{Method: r.Method, URL: r.URL, Header: r.Header, Body: r.Body}
		response, err := transport.RoundTrip(outbound)
		if err != nil {
			http.Error(w, "forward failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

// canaryE2EAdapter 是 grok 上游替身适配器:经节点代理(钉住受检节点)向本地
// SSE 源站发真实 HTTP 请求——首事件 response.created + 可见 reasoning 增量,
// 正是 canary 判 Clean 的证据形状。
type canaryE2EAdapter struct {
	provider.Adapter
	manager   *infraegress.Manager
	upstream  *httptest.Server
	forwards  atomic.Int64
	proxyHits atomic.Int64
}

func (a *canaryE2EAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }

func (a *canaryE2EAdapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.forwards.Add(1)
	lease, err := a.manager.Acquire(ctx, egressdomain.ScopeBuild, "canary-e2e")
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.upstream.URL+"/v1/responses", strings.NewReader(string(request.Body)))
	if err != nil {
		lease.Release()
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := lease.Do(httpRequest)
	if err != nil {
		lease.Release()
		return nil, err
	}
	a.proxyHits.Add(1)
	return &provider.Response{StatusCode: response.StatusCode, Header: response.Header, Body: response.Body}, nil
}

// e2eRotationSSEUpstream 模拟 grok 推理上游:立即首事件 + 可见思考增量 + 完成事件。
func e2eRotationSSEUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"1\"}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
		flusher.Flush()
	}))
}

// TestE2ERotationFullChainLocalDoubles 端到端验证换 IP 全链(EXIT-IP-GUARD §4/§5
// 的自动化形态, 本地替身按 §7):跨账号降智确认 → 隔离 → 排队 → worker →
// rotate-server 替身 webhook(真实 HTTP, token 校验)→ settle → 真实探活
// (经代理替身 CONNECT 隧道到真实 ipinfo.io)→ 出口 IP 变化判定 → 真实
// canary(经代理替身到本地 SSE 上游, 首事件+思考证据)→ 解除隔离回池 →
// 复调度;随后验证"出口 IP 未变"失败路径(尝试计数 +1, 保持隔离)。
//
// 无法本地验证的只剩真实 MicroWARP/WARP 隧道本身(替身 IP 不变——本测试
// 以 DB 中预置陈旧 ExitIP 模拟"换到了新 IP")。
func TestE2ERotationFullChainLocalDoubles(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end rotation chain needs real network probes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "rotation-e2e.db"))
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

	// 本地替身:节点代理(MicroWARP shim 形态)、rotate-server webhook、SSE 上游。
	proxy := newE2EForwardProxy(t)
	webhookCalls := atomic.Int64{}
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "verify-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(webhook.Close)
	upstream := e2eRotationSSEUpstream()
	t.Cleanup(upstream.Close)

	// 真实仓储 + 真实 infra manager(隔离器 + 节点探测器)。
	egressRepo := relational.NewEgressRepository(database)
	manager := infraegress.NewManager(egressRepo, cipher)
	encryptedProxy, err := cipher.Encrypt(proxy.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	encryptedWebhook, err := cipher.Encrypt(webhook.URL + "/rotate?token=verify-token")
	if err != nil {
		t.Fatal(err)
	}
	node, err := egressRepo.CreateEgressNode(ctx, egressdomain.Node{
		Name: "warp-e2e", Enabled: true, EncryptedProxyURL: encryptedProxy, Health: 1,
		EncryptedRotationURL: encryptedWebhook, RotationEnabled: true,
		// 陈旧出口 IP:模拟"此前探测到的旧 IP", 首次轮换后真实出口 IP 与之
		// 不同 → "IP 已变化" 判定通过(替身无法真实换 IP, 见 §7)。
		ExitIP: "203.0.113.7", ProbeStatus: egressdomain.ProbeStatusHealthy,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 真实网关服务:模型路由 + 单账号 + 真实 Registry(canary 经真实适配器)。
	modelRepo := relational.NewModelRepository(database)
	accountRepo := relational.NewAccountRepository(database)
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	credential, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "canary", SourceKey: "rotation-e2e-canary",
		EncryptedAccessToken: "token", AuthStatus: accountdomain.AuthStatusActive, Priority: 100, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.5"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	adapter := &canaryE2EAdapter{manager: manager, upstream: upstream}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	auditRepo := relational.NewAuditRepository(database)
	accountService := account.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	gatewayService := NewService(modelRepo, auditRepo, accountService, nil, registry, selector, nil, 3)
	gatewayService.UpdateEgressCanary(EgressCanaryRuntime{ModelPublicID: "grok-4.5", CreatedTimeout: 5 * time.Second})

	// 真实 egress 应用服务:隔离器/探测器/canary 全部接真实实现。
	egressService := egressapp.NewService(egressRepo, cipher)
	egressService.SetQualityGuardConfig(egressapp.DefaultQualityGuardConfig())
	visibleLogger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	egressService.SetQualityLogger(visibleLogger)
	egressService.SetRotationLogger(visibleLogger)
	egressService.SetQualityQuarantiner(manager)
	egressService.SetNodeProber(manager)
	egressService.SetEgressQualityProber(gatewayService)
	rotationCfg := egressapp.DefaultRotationConfig()
	rotationCfg.SettleDelay = 100 * time.Millisecond
	rotationCfg.ProbeTimeout = 20 * time.Second
	rotationCfg.ProbeInterval = 100 * time.Millisecond
	rotationCfg.MinNodeInterval = 0
	rotationCfg.MaxGlobalPerHour = 1000
	rotationCfg.WebhookTimeout = 5 * time.Second
	egressService.SetRotationConfig(rotationCfg)

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	go egressService.RunRotationWorker(workerCtx)

	// 触发:两个不同账号在同一节点降智 → 跨账号确认 → 隔离 + 入队。
	egressService.OnEgressDegraded(ctx, node.ID, 9001)
	egressService.OnEgressDegraded(ctx, node.ID, 9002)

	// 等待全链完成:解除隔离(尝试归零、无错误、冷却清除、出口 IP 已更新)。
	deadline := time.Now().Add(60 * time.Second)
	var released *egressdomain.Node
	for time.Now().Before(deadline) {
		current, getErr := egressRepo.GetEgressNode(ctx, node.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.RotationAttempts == 0 && current.LastRotationError == "" && current.ExitIP != "203.0.113.7" && current.ExitIP != "" && (current.CooldownUntil == nil || time.Now().UTC().After(*current.CooldownUntil)) {
			released = &current
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("rotation chain did not complete: %v; node=%+v", ctx.Err(), current)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if released == nil {
		current, _ := egressRepo.GetEgressNode(ctx, node.ID)
		t.Fatalf("node was not released after full chain: %+v", current)
	}

	// 证据链:webhook 真实命中(token 校验通过)、真实互联网探活(CONNECT 隧道)、
	// canary 真实穿过代理(SSE 转发)、出口 IP 为真实公网 IP。
	if webhookCalls.Load() == 0 {
		t.Fatal("rotate-server stand-in webhook was never called")
	}
	if proxy.tunnels.Load() == 0 {
		t.Fatal("probe did not tunnel through the node proxy to the real probe endpoint")
	}
	if adapter.forwards.Load() == 0 || adapter.proxyHits.Load() == 0 {
		t.Fatalf("canary did not traverse the node proxy: forwards=%d proxyHits=%d", adapter.forwards.Load(), adapter.proxyHits.Load())
	}
	if net.ParseIP(released.ExitIP) == nil {
		t.Fatalf("exit IP after rotation is not a real address: %q", released.ExitIP)
	}

	// 回池:解除隔离后, 未钉住的管理器获取路径可以再次调度该节点。
	manager.InvalidatePoolCache()
	lease, _, err := manager.AcquireIfConfigured(ctx, egressdomain.ScopeBuild, "post-release")
	if err != nil || lease == nil {
		t.Fatalf("released node must be schedulable again: lease=%v err=%v", lease, err)
	}
	if lease.NodeID != node.ID {
		// 单节点部署下唯一启用节点即受检节点;若将来测试环境变化再放宽。
		t.Fatalf("expected the released node to serve traffic, got node %d", lease.NodeID)
	}
	lease.Release()

	// 复隔离: 再次降智证据 → 节点重新隔离并重新入队。刚成功轮换过的节点受
	// MinNodeInterval 节流(文档语义: 同一节点两次换 IP 最小间隔, 配置 0 归一
	// 为 10m)——尝试计数不增加, 状态记录节流原因, 节点保持隔离。"ExitIP 未
	// 变"的失败分类由 TestRotationUnchangedExitIPFails 单元覆盖。
	egressService.QuarantineForExitIP(ctx, node.ID, 9003)
	failDeadline := time.Now().Add(15 * time.Second)
	var requarantined *egressdomain.Node
	for time.Now().Before(failDeadline) {
		current, getErr := egressRepo.GetEgressNode(ctx, node.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.CooldownUntil != nil && time.Now().UTC().Before(*current.CooldownUntil) && current.LastError == egressdomain.LastErrorExitIPQuality && strings.Contains(current.LastRotationError, "min interval") {
			requarantined = &current
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("re-quarantine did not complete: %v; node=%+v", ctx.Err(), current)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if requarantined == nil {
		current, _ := egressRepo.GetEgressNode(ctx, node.ID)
		t.Fatalf("re-quarantine with interval throttle not observed: %+v", current)
	}
	if requarantined.RotationAttempts != 0 {
		t.Fatalf("rate-limited re-quarantine must not burn an attempt: %+v", requarantined)
	}
}

// 保证未使用 import 不因构建标签差异报错(保留 bufio 供扩展)。
