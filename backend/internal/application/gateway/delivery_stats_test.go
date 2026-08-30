package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestDeliveredStatsRecordedFromTransportCallback 锁定轮26 交付统计的
// service 侧链路：Result.RecordDelivery 回调（transport 在转发完成后调用）
// 的值必须进入审计行的 DeliveredEvents/DeliveredBytes。transport 侧计数
// 由部署实例冒烟与审计行核对覆盖（见提交信息）。
func TestDeliveredStatsRecordedFromTransportCallback(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "delivery-cb.db"))
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

	acc, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "d", SourceKey: "d", EncryptedAccessToken: "one", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, acc.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{Name: "dk", Prefix: "dk", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "enc", Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}

	stream := "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"think\"}\n\ndata: [DONE]\n\n"
	adapter := &deliveryAdapter{sse: stream}
	registry := provider.NewRegistry(adapter)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	clientService := clientkeyapp.NewService(nil, nil, nil, 60, 4, nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientService, registry, selector, responseRepo, 3)
	service.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, MaxAttempts: 2, OnExhausted: qualityRetryFailClosed, EvidenceTimeout: 400 * time.Millisecond, CreatedTimeout: 300 * time.Millisecond})

	result, err := service.CreateResponse(ctx, Input{RequestID: "deliv-cb", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true, Body: []byte("{\"model\":\"grok-4.6\",\"stream\":true,\"input\":\"hi\"}")})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 transport：读体后在 Finalize 前调用 RecordDelivery。
	_, _ = io.Copy(io.Discard, result.Body)
	if result.RecordDelivery == nil {
		t.Fatal("RecordDelivery 未装配")
	}
	result.RecordDelivery(DeliveryStats{Events: 7, Bytes: 4096})
	result.Finalize(Usage{}, "r1", "")
	_ = result.Body.Close()

	logs, total, err := auditRepo.List(ctx, 0, 5)
	if err != nil || total != 1 {
		t.Fatalf("audits=%d err=%v", total, err)
	}
	if logs[0].DeliveredEvents != 7 || logs[0].DeliveredBytes != 4096 {
		t.Fatalf("交付统计 = (%d, %d), want (7, 4096)", logs[0].DeliveredEvents, logs[0].DeliveredBytes)
	}
}

type deliveryAdapter struct {
	sse string
}

func (a *deliveryAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *deliveryAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (a *deliveryAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	header := make(http.Header)
	header.Set("Content-Type", "text/event-stream")
	return &provider.Response{StatusCode: 200, Status: "200 OK", Header: header, Body: io.NopCloser(strings.NewReader(a.sse))}, nil
}
func (a *deliveryAdapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return nil, nil
}
func (a *deliveryAdapter) GetBilling(context.Context, account.Credential) (account.Billing, error) {
	return account.Billing{}, nil
}
func (a *deliveryAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, nil
}
func (a *deliveryAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, nil
}
func (a *deliveryAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return provider.CredentialSeed{}, nil
}
func (a *deliveryAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return nil, nil
}
func (a *deliveryAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}
