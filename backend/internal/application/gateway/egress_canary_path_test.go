package gateway

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

	"github.com/chenyme/grok2api/backend/internal/application/account"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	cliadapter "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestEgressCanaryPostsToResponsesPath 回归(2026-08-25 线上事故):canary 请求
// 曾漏设 Path,适配器按 urlWithBase(base, "") 把请求 POST 到 base 根路径——
// cli-chat-proxy.grok.com 对 /v1/ 恒 404,canary 因此对任何出口都判
// degraded:换 IP 明明成功也被烧满 3 次尝试、耗尽后保持 24h 隔离。
// 既有 e2e 用替身适配器(硬编码 /v1/responses 路径)从未覆盖真实 URL 构造,
// 这里改用真实 Build 适配器断言物理请求路径与 Clean 判定。
func TestEgressCanaryPostsToResponsesPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "canary-path.db"))
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

	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"1\"}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	modelRepo := relational.NewModelRepository(database)
	accountRepo := relational.NewAccountRepository(database)
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	encryptedToken, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "canary-path", SourceKey: "canary-path",
		EncryptedAccessToken: encryptedToken, AuthStatus: accountdomain.AuthStatusActive, Priority: 100, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.5"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	adapter := cliadapter.NewAdapter(cliadapter.Config{
		BaseURL: upstream.URL + "/v1", ClientVersion: "0.2.102", ClientIdentifier: "grok-shell",
		TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.102 (linux; x86_64)",
	}, cipher)
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	auditRepo := relational.NewAuditRepository(database)
	accountService := account.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	gatewayService := NewService(modelRepo, auditRepo, accountService, nil, registry, selector, nil, 3)
	gatewayService.UpdateEgressCanary(EgressCanaryRuntime{ModelPublicID: "grok-4.5", CreatedTimeout: 5 * time.Second})

	result := gatewayService.ProbeEgressQuality(ctx, 0)
	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("canary never reached the upstream: result=%+v", result)
	}
	for _, path := range got {
		// base 含 /v1 前缀, 完整路径应为 <base>/responses;退化成 base 根
		// (仅 /v1/) 即为漏 Path 的 404 回归。
		if !strings.HasSuffix(path, "/responses") {
			t.Fatalf("canary posted to %q, want <base>/responses (base-root 404 regression); paths=%v result=%+v", path, got, result)
		}
	}
	if result.Outcome != egressapp.EgressQualityProbeClean {
		t.Fatalf("canary outcome = %s (%s), want clean", result.Outcome, result.Reason)
	}
}
