package gateway

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// qualityChainAdapter 按账号脚本化上游流：degradedID 收到无思考的降智
// Responses 流，其余账号收到带可见思考增量的 clean 流。两种流都带唯一
// 内容标记，客户端字节纯度断言据此判定扣留的降智前缀是否泄漏。
type qualityChainAdapter struct {
	mu          sync.Mutex
	degradedID  uint64
	attempts    []uint64
	degradedSSE string
	cleanSSE    string
}

func (a *qualityChainAdapter) Provider() account.Provider { return account.ProviderBuild }

func (a *qualityChainAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderBuild)
	return definition
}

func (a *qualityChainAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	degraded := request.Credential.ID == a.degradedID
	body := a.cleanSSE
	if degraded {
		body = a.degradedSSE
	}
	a.mu.Unlock()
	header := make(http.Header)
	header.Set("Content-Type", "text/event-stream")
	return &provider.Response{StatusCode: 200, Status: "200 OK", Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (a *qualityChainAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

func (a *qualityChainAdapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return nil, nil
}
func (a *qualityChainAdapter) GetBilling(context.Context, account.Credential) (account.Billing, error) {
	return account.Billing{}, nil
}
func (a *qualityChainAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, nil
}
func (a *qualityChainAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, nil
}
func (a *qualityChainAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return provider.CredentialSeed{}, nil
}
func (a *qualityChainAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return nil, nil
}
func (a *qualityChainAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}

// TestQualityGuardTransparentFailoverChain 是守卫核心承诺的 Service 级
// 端到端锁：高优先级账号降智（无思考流）→ 守卫扣留 → missing-thinking
// 惩罚冷却 → 换号 → clean 账号交付。客户端必须只看到 clean 流的字节
// （降智前缀零泄漏），且后续请求因冷却直接绕开降智账号。
func TestQualityGuardTransparentFailoverChain(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)

	degraded, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "degraded", SourceKey: "degraded", EncryptedAccessToken: "one", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	clean, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "clean", SourceKey: "clean", EncryptedAccessToken: "two", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []uint64{degraded.ID, clean.ID} {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, accountID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{Name: "q-key", Prefix: "q-prefix", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "enc", Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}

	degradedSSE := strings.Join([]string{
		"data: {" + "\"type\":\"response.created\",\"response\":{\"id\":\"resp_deg\"}}",
		"data: {" + "\"type\":\"response.output_text.delta\",\"delta\":\"DEGRADED-LEAK-MARKER-A word word word\"}",
		"data: {" + "\"type\":\"response.completed\",\"response\":{\"id\":\"resp_deg\",\"usage\":{\"output_tokens\":50}}}",
		"data: [DONE]",
	}, "\n\n") + "\n\n"
	cleanSSE := strings.Join([]string{
		"data: {" + "\"type\":\"response.created\",\"response\":{\"id\":\"resp_clean\"}}",
		"data: {" + "\"type\":\"response.reasoning_text.delta\",\"delta\":\"plan before answer\"}",
		"data: {" + "\"type\":\"response.output_text.delta\",\"delta\":\"CLEAN-DELIVERY-MARKER-B answer\"}",
		"data: {" + "\"type\":\"response.completed\",\"response\":{\"id\":\"resp_clean\",\"usage\":{\"output_tokens\":40,\"output_tokens_details\":{\"reasoning_tokens\":10}}}}",
		"data: [DONE]",
	}, "\n\n") + "\n\n"

	adapter := &qualityChainAdapter{degradedID: degraded.ID, degradedSSE: degradedSSE, cleanSSE: cleanSSE}
	registry := provider.NewRegistry(adapter)
	cipher := testCipher(t)
	sticky := memory.NewStickyStore()
	concurrency := memory.NewConcurrencyLimiter()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	clientService := clientkeyapp.NewService(nil, nil, nil, 60, 4, nil)
	selector := NewSelector(accountRepo, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientService, registry, selector, responseRepo, 3)
	service.UpdateQualityRetry(QualityRetryRuntime{
		Enabled: true, MaxAttempts: 3,
		OnExhausted:     qualityRetryFailClosed,
		EvidenceTimeout: 400 * time.Millisecond, CreatedTimeout: 300 * time.Millisecond,
		SameAccountRetry: false, AccountCooldown: 12 * time.Hour,
	})

	// 请求 1：降智账号（高优先级）→ 扣留 → 换 clean 账号交付。
	first, err := service.CreateResponse(ctx, Input{
		RequestID: "q-chain-1", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte("{\"model\":\"grok-4.6\",\"stream\":true,\"input\":\"hello\"}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatal(err)
	}
	first.MarkFirstToken()
	first.Finalize(Usage{Reported: true, InputTokens: 10, OutputTokens: 40, ReasoningTokens: 10}, "resp_clean", "")
	_ = first.Body.Close()

	if got := adapter.Attempts(); len(got) != 2 || got[0] != degraded.ID || got[1] != clean.ID {
		t.Fatalf("请求1 尝试序列 = %#v，应 [降智→clean]", got)
	}
	if string(body) != cleanSSE {
		t.Fatalf("客户端字节不纯：期望逐字节等于 clean 流（%d B），得到 %d B；含降智标记=%v",
			len(cleanSSE), len(body), strings.Contains(string(body), "DEGRADED-LEAK-MARKER-A"))
	}

	// 请求 2：降智账号已因 missing-thinking 惩罚冷却，应直接命中 clean 账号。
	second, err := service.CreateResponse(ctx, Input{
		RequestID: "q-chain-2", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte("{\"model\":\"grok-4.6\",\"stream\":true,\"input\":\"again\"}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	body2, err := io.ReadAll(second.Body)
	if err != nil {
		t.Fatal(err)
	}
	second.MarkFirstToken()
	second.Finalize(Usage{Reported: true, InputTokens: 10, OutputTokens: 40, ReasoningTokens: 10}, "resp_clean", "")
	_ = second.Body.Close()

	got := adapter.Attempts()
	if len(got) != 3 || got[2] != clean.ID {
		t.Fatalf("请求2 尝试序列 = %#v，惩罚冷却后应直接命中 clean 账号（共3次尝试）", got)
	}
	if string(body2) != cleanSSE {
		t.Fatalf("请求2 客户端字节不纯：含降智标记=%v", strings.Contains(string(body2), "DEGRADED-LEAK-MARKER-A"))
	}
}
