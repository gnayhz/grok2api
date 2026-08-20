package gateway

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
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

// TestNonRetryable4xxAfterAccountSwitchStaysSanitized locks the P0 fix from
// the adversarial review: a retryable first attempt (429) sets lastFailure;
// the follow-up account then returns a non-retryable 400. The sanitization
// branch used to require lastFailure == nil, so the raw upstream body was
// handed to the client verbatim after the switch. The envelope must win.
func TestNonRetryable4xxAfterAccountSwitchStaysSanitized(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "leak-4xx-switch.db"))
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

	first, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "switch-first", SourceKey: "switch-first",
		EncryptedAccessToken: "first", EncryptedRefreshToken: "refresh",
		ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 100, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "switch-second", SourceKey: "switch-second",
		EncryptedAccessToken: "second", EncryptedRefreshToken: "refresh",
		ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 90, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, first.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, second.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "switch-key", Prefix: "sw", SecretHash: strings.Repeat("b", 64), EncryptedSecret: "enc",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	// First account answers 429 (retryable, sets lastFailure), second answers
	// the non-retryable 400 carrying upstream internals.
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		first.ID:  {{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`}},
		second.ID: {{status: http.StatusBadRequest, body: `{"error":{"message":"invalid request trace-id=secret-internal-abc session-token-hint=xyz","type":"invalid_request_error"}}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-leak-4xx-switch", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: false,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err == nil {
		_ = result.Body.Close()
		t.Fatal("post-switch non-retryable 4xx must surface as an UpstreamFailure, not a delivered body")
	}
	var failure *UpstreamFailure
	if !errors.As(err, &failure) {
		t.Fatalf("failure must be *UpstreamFailure, got %T: %v", err, err)
	}
	if failure.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 preserved for the client", failure.HTTPStatus)
	}
	if strings.Contains(failure.PublicMessage, "secret-internal-abc") || strings.Contains(failure.PublicMessage, "session-token-hint") {
		t.Fatalf("public message must not echo upstream internals: %q", failure.PublicMessage)
	}
}
