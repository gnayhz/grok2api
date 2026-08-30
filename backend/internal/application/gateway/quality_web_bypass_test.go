package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkey "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// webNoThinkingStreamAdapter 返回 Web 原生 chat 形态流：有正文、零思考
// 字段——若守卫错误地介入 Web 供应商，该形态必被判扣留。
type webNoThinkingStreamAdapter struct {
	calls atomic.Int64
}

func (a *webNoThinkingStreamAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderWeb
}
func (a *webNoThinkingStreamAdapter) Definition() provider.Definition {
	return testConversationDefinition(accountdomain.ProviderWeb)
}
func (a *webNoThinkingStreamAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	a.calls.Add(1)
	body := strings.Join([]string{
		"data: " + `{"choices":[{"delta":{"content":"web native answer without reasoning fields"}}]}`,
		"data: [DONE]",
	}, "\n\n")
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

// TestWebProviderStreamBypassesQualityHold：Web 供应商豁免是形状必需而非
// 偏好——Web 流没有思考证据通道，若守卫介入会 100% 扣留。服务级锁定：
// 守卫全开时 Web 流式请求单次尝试直接交付。
func TestWebProviderStreamBypassesQualityHold(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, t.TempDir()+"/web-bypass-hold.db")
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, WebTier: accountdomain.WebTierSuper,
		Name: "web-bypass", SourceKey: "web-bypass", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const model = "grok-web-bypass"
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderWeb, []string{model}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	adapter := &webNoThinkingStreamAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, nil, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)
	service.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, MaxAttempts: 2, OnExhausted: qualityRetryFailClosed})

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-web-bypass", ClientKey: clientkey.Key{ID: 1, Name: "web"}, PublicModel: model, Streaming: true,
		Body: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	})
	if err != nil {
		t.Fatalf("web stream must bypass the guard, err=%v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (guard must not engage for web provider)", result.StatusCode)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if calls := adapter.calls.Load(); calls != 1 {
		t.Fatalf("adapter calls = %d, want exactly 1 (no withhold retry)", calls)
	}
}
