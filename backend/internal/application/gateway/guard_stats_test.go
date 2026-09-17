package gateway

import (
	"context"
	"encoding/json"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

// TestGuardStatsCountRescuedAndFailedWithhold 验证守卫特征统计的端到端
// 语义:扣留(missing_thinking)触发的请求,一次被后续账号救回(Rescued+1),
// 一次 fail-closed 耗尽拒绝(Failed+1 + ExhaustedRejected+1)。两个场景用
// 不同模型路由隔离账号池,避免选号顺序/账号冷却互相污染;计数为包级单例,
// 断言取前后差值。
func TestGuardStatsCountRescuedAndFailedWithhold(t *testing.T) {
	before := processGuardStatsSnapshot()
	signalBefore := findGuardSignalStat(t, before, GuardSignalWithhold)
	retrialBefore := before.Retrial

	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "guard-stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)

	makeAccount := func(name string, priority int, models []string) accountdomain.Credential {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name,
			EncryptedAccessToken: name, EncryptedRefreshToken: "refresh-" + name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
			Priority: priority, MaxConcurrent: 4,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if err := testsupport.Capabilities(ctx, modelRepo, accountRepo, credential.ID, models, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		return credential
	}
	if err := testsupport.Discover(ctx, modelRepo, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	// 统一 grok-4.6 池,用优先级控制选号:场景二先跑,选中 failClosed(300)
	// 扣留拒绝并进入 12h 冷却;场景一随后选中 degraded(200) 扣留,重试落到
	// clean(100) 救回。
	failClosedAccount := makeAccount("guard-stats-failclosed", 300, []string{"grok-4.6"})
	degradedAccount := makeAccount("guard-stats-degraded", 200, []string{"grok-4.6"})
	cleanAccount := makeAccount("guard-stats-clean", 100, []string{"grok-4.6"})
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{ModelScope: clientkey.ModelScopeAll,
		Name: "guard-stats-key", Prefix: "gstats", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	degraded := sse(
		`data: {"choices":[{"delta":{"content":"`+strings.Repeat("word ", 40)+`"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":40}}}`,
		"data: [DONE]",
	)
	clean := sse(
		`data: {"choices":[{"delta":{"reasoning_content":"thinking through"}}]}`,
		`data: {"choices":[{"delta":{"content":"final answer"}}]}`,
		"data: [DONE]",
	)

	newService := func(responses map[uint64][]scriptedBuildResponse, maxAttempts int) *Service {
		adapter := &scriptedBuildAdapter{responses: responses}
		registry := providerimpl.NewRegistry(adapter)
		sticky := memory.NewStickyStore()
		accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), security.RandomTokenSource{}, nil, nil, nil)
		sel := selector.NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
		service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService("test-owner", nil, nil, nil, 60, 4, nil, security.RandomTokenSource{}), registry, sel, historyapp.NewResponseResources(responseRepo), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 999)
		service.SetGuardSnapshotSource(StaticGuardSnapshotSource(QualityRetryRuntime{
			Enabled: true, MaxAttempts: maxAttempts,
			OnExhausted: qualityRetryFailClosed, GuardedModels: []string{"grok-4.6"},
		}))
		return service
	}
	chatInput := func(requestID, model string) Input {
		body, buildErr := json.Marshal(map[string]any{
			"model": model, "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "answer"}},
		})
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		return Input{RequestID: requestID, ClientKey: clientKey, PublicModel: model, Streaming: true, Body: body}
	}

	// 场景二(先跑):任何选中都是降智流,预算 1 次 → fail-closed 拒绝。
	failedService := newService(map[uint64][]scriptedBuildResponse{
		failClosedAccount.ID: {{status: http.StatusOK, body: degraded}},
		degradedAccount.ID:   {{status: http.StatusOK, body: degraded}},
		cleanAccount.ID:      {{status: http.StatusOK, body: degraded}},
	}, 1)
	if _, err := failedService.CreateChatCompletion(ctx, chatInput("req-guard-stats-failed", "grok-4.6")); err == nil {
		t.Fatal("fail-closed scenario must return the quality failure")
	}

	// 场景一使用新的嵌入实例；显式继承前一场景的本地临时限制。
	rescuedService := newService(map[uint64][]scriptedBuildResponse{
		degradedAccount.ID: {{status: http.StatusOK, body: degraded}},
		cleanAccount.ID:    {{status: http.StatusOK, body: clean}},
	}, 3)
	rescuedService.selector.HoldLocalQuality(failClosedAccount.ID, "previous-scenario", time.Now().Add(time.Minute))
	result, err := rescuedService.CreateChatCompletion(ctx, chatInput("req-guard-stats-rescued", "grok-4.6"))
	if err != nil {
		t.Fatalf("rescued scenario must deliver: %v", err)
	}
	_, _ = io.Copy(io.Discard, result.Body)
	finishTestResult(t, result, Usage{}, "", "")
	_ = result.Body.Close()

	after := processGuardStatsSnapshot()
	signalAfter := findGuardSignalStat(t, after, GuardSignalWithhold)
	if signalAfter.Triggered-signalBefore.Triggered != 2 {
		t.Fatalf("triggered delta = %d, want exactly 2 (both scenarios withhold)", signalAfter.Triggered-signalBefore.Triggered)
	}
	if signalAfter.Rescued-signalBefore.Rescued != 1 {
		t.Fatalf("rescued delta = %d, want exactly 1", signalAfter.Rescued-signalBefore.Rescued)
	}
	if signalAfter.Failed-signalBefore.Failed != 1 {
		t.Fatalf("failed delta = %d, want exactly 1", signalAfter.Failed-signalBefore.Failed)
	}
	if after.Retrial.ExhaustedRejected-retrialBefore.ExhaustedRejected != 1 {
		t.Fatalf("exhaustedRejected delta = %d, want 1", after.Retrial.ExhaustedRejected-retrialBefore.ExhaustedRejected)
	}
}

func findGuardSignalStat(t *testing.T, snapshot GuardStatsSnapshot, signal GuardSignal) GuardSignalStat {
	t.Helper()
	for _, stat := range snapshot.Signals {
		if stat.Signal == string(signal) {
			return stat
		}
	}
	t.Fatalf("signal %s missing from snapshot", signal)
	return GuardSignalStat{}
}
