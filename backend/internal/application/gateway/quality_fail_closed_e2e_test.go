package gateway

import (
	"context"
	"errors"
	"fmt"
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

// TestAttemptLoopQualityFailClosedRejectsAndNeverLeaksDegradedBytes is the
// fail-closed counterpart of the fail-open E2E cap test: every eligible
// reasoning account returns a terminal no-think stream (content plus a usage
// frame CLAIMING reasoning tokens — the laundering shape), the budget
// exhausts under OnExhausted=fail_closed, and the request loop must surface
// a quality 503 while zero degraded SSE bytes reach the caller (test-quality
// audit P0 gap: only unit reject decisions were covered before).
func TestAttemptLoopQualityFailClosedRejectsAndNeverLeaksDegradedBytes(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-failclosed.db"))
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

	const accounts = 3
	credentials := make([]accountdomain.Credential, 0, accounts)
	for index := 0; index < accounts; index++ {
		name := fmt.Sprintf("quality-failclosed-%d", index)
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name,
			EncryptedAccessToken: name, EncryptedRefreshToken: "refresh-" + name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
			Priority: 300 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "quality-failclosed-key", Prefix: "qfclosed", SecretHash: strings.Repeat("f", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Degraded no-think body per account: terminal content plus a usage frame
	// that CLAIMS reasoning tokens. Text-delta evidence is absent, so the
	// usage claim must not launder the stream (R2) and every attempt withholds.
	degraded := sse(
		`data: {"choices":[{"delta":{"content":"`+strings.Repeat("word ", 40)+`"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":40}}}`,
		"data: [DONE]",
	)
	responses := make(map[uint64][]scriptedBuildResponse, accounts)
	for _, credential := range credentials {
		responses[credential.ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degraded}}
	}
	adapter := &scriptedBuildAdapter{responses: responses}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 999)
	service.UpdateQualityRetry(QualityRetryRuntime{
		Enabled: true, MaxAttempts: accounts, MinOutputTokens: 32,
		OnExhausted: qualityRetryFailClosed, HoldTimeout: time.Second,
	})

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-quality-failclosed", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"answer"}],"stream":true}`),
	})
	if err == nil {
		_, _ = io.ReadAll(result.Body)
		_ = result.Body.Close()
		t.Fatal("fail-closed exhaustion must return the quality failure, not a result")
	}
	var failure *UpstreamFailure
	if !errors.As(err, &failure) {
		t.Fatalf("failure must be *UpstreamFailure, got %T: %v", err, err)
	}
	if failure.HTTPStatus != http.StatusServiceUnavailable || failure.Code != ErrorQualityDegraded {
		t.Fatalf("failure = %d/%s, want 503/%s", failure.HTTPStatus, failure.Code, ErrorQualityDegraded)
	}

	// Every eligible account was attempted exactly once and withheld: the
	// budget exhausted through real switches, not an early bail-out.
	attempts := adapter.Attempts()
	if len(attempts) != accounts {
		t.Fatalf("attempts = %d, want %d (budget must exhaust across accounts)", len(attempts), accounts)
	}
	seen := make(map[uint64]bool, accounts)
	for _, id := range attempts {
		if seen[id] {
			t.Fatalf("account %d attempted twice under fail-closed", id)
		}
		seen[id] = true
	}

	// Every withheld account is cooling with the missing-thinking marker.
	for _, credential := range credentials {
		account, getErr := accountRepo.Get(ctx, credential.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if account.CooldownUntil == nil || account.LastError != lastErrorMissingThinking {
			t.Fatalf("account %d must cool with missing-thinking, got until=%v last=%q", credential.ID, account.CooldownUntil, account.LastError)
		}
	}

	// One parent 503; each withheld account is an attempt on that row.
	logs, total, listErr := auditRepo.List(ctx, 0, 50)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if total != 1 {
		t.Fatalf("fail-closed must write one audit parent, got %d", total)
	}
	if logs[0].ErrorCode != ErrorQualityDegraded || logs[0].StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("parent = %d/%s, want 503/%s", logs[0].StatusCode, logs[0].ErrorCode, ErrorQualityDegraded)
	}
	detail, getErr := auditRepo.Get(ctx, logs[0].ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	holds := 0
	for _, attempt := range detail.Attempts {
		if attempt.TransportError == ErrorQualityDegraded {
			holds++
		}
	}
	if holds != accounts {
		t.Fatalf("quality_hold attempts = %d, want %d", holds, accounts)
	}
}
