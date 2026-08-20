package gateway

import (
	"context"
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
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// TestNonRetryableUpstream4xxNeverLeaksRawBody: a 400 with an upstream
// internal-trace body used to be forwarded verbatim to inference clients
// (only 401/402/403 had dedicated handling). The sanitized envelope must
// carry the status and a generic message while the raw body stays internal
// (adversarial security review P1).
func TestNonRetryableUpstream4xxNeverLeaksRawBody(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "leak-4xx.db"))
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

	credential, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "leak-acct", SourceKey: "leak-acct",
		EncryptedAccessToken: "leak", EncryptedRefreshToken: "refresh",
		ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 100, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "leak-key", Prefix: "leak", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "enc",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credential.ID: {{status: http.StatusBadRequest, body: `{"error":{"message":"invalid request trace-id=secret-internal-abc session-token-hint=xyz","type":"invalid_request_error"}}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-leak-4xx", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: false,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err == nil {
		_ = result.Body.Close()
		t.Fatal("non-retryable upstream 4xx must surface as an UpstreamFailure, not a delivered body")
	}
	var failure *UpstreamFailure
	if !errorsAs(err, &failure) {
		t.Fatalf("failure must be *UpstreamFailure, got %T: %v", err, err)
	}
	if failure.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 preserved for the client", failure.HTTPStatus)
	}
	if strings.Contains(failure.PublicMessage, "secret-internal-abc") || strings.Contains(failure.PublicMessage, "session-token-hint") {
		t.Fatalf("public message must not echo upstream internals: %q", failure.PublicMessage)
	}
}

func errorsAs(err error, target **UpstreamFailure) bool {
	for err != nil {
		if f, ok := err.(*UpstreamFailure); ok {
			*target = f
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var _ = io.Discard
