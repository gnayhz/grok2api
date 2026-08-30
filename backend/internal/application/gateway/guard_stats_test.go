package gateway

import (
	"context"
	"encoding/json"
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
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// TestGuardStatsCountRescuedAndFailedWithhold 验证守卫特征统计的端到端
// 语义:扣留(missing_thinking)触发的请求,一次被后续账号救回(Rescued+1),
// 一次 fail-closed 耗尽拒绝(Failed+1 + ExhaustedRejected+1)。两个场景用
// 不同模型路由隔离账号池,避免选号顺序/账号冷却互相污染;计数为包级单例,
// 断言取前后差值。
func TestGuardStatsCountRescuedAndFailedWithhold(t *testing.T) {
	before := GuardStatsSnapshotForAPI()
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
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, models, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		return credential
	}
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	// 统一 grok-4.6 池,用优先级控制选号:场景二先跑,选中 failClosed(300)
	// 扣留拒绝并进入 12h 冷却;场景一随后选中 degraded(200) 扣留,重试落到
	// clean(100) 救回。
	failClosedAccount := makeAccount("guard-stats-failclosed", 300, []string{"grok-4.6"})
	degradedAccount := makeAccount("guard-stats-degraded", 200, []string{"grok-4.6"})
	cleanAccount := makeAccount("guard-stats-clean", 100, []string{"grok-4.6"})
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
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
		registry := provider.NewRegistry(adapter)
		sticky := memory.NewStickyStore()
		accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
		selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
		service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 999)
		service.UpdateQualityRetry(QualityRetryRuntime{
			Enabled: true, MaxAttempts: maxAttempts,
			OnExhausted: qualityRetryFailClosed,
		})
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

	// 场景一(后跑):failClosed 已冷却,degraded(200) 扣留 → clean 救回。
	rescuedService := newService(map[uint64][]scriptedBuildResponse{
		degradedAccount.ID: {{status: http.StatusOK, body: degraded}},
		cleanAccount.ID:    {{status: http.StatusOK, body: clean}},
	}, 3)
	result, err := rescuedService.CreateChatCompletion(ctx, chatInput("req-guard-stats-rescued", "grok-4.6"))
	if err != nil {
		t.Fatalf("rescued scenario must deliver: %v", err)
	}
	_ = result.Body.Close()

	after := GuardStatsSnapshotForAPI()
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
