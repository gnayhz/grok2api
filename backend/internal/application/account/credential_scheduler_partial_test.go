package account

import (
	"context"
	"fmt"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestRefreshDueCredentialsContinuesAfterPartialBatchFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	service, poison, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return time.Now().UTC() }
	adapter.failSourceKey = poison.SourceKey

	early := now.Add(-2 * time.Minute)
	poison.RefreshDueAt = &early
	if _, err := service.accounts.Update(ctx, poison); err != nil {
		t.Fatal(err)
	}

	later := now.Add(-time.Minute)
	var lastID uint64
	for i := 0; i < credentialRefreshBatchSize; i++ {
		created, _, err := service.accounts.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: fmt.Sprintf("due-%d", i), SourceKey: fmt.Sprintf("due-%d", i),
			EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: now.Add(time.Hour),
			Enabled: true, AuthStatus: accountdomain.AuthStatusActive, RefreshDueAt: &later,
		})
		if err != nil {
			t.Fatal(err)
		}
		lastID = created.ID
	}

	if err := service.refreshDueCredentials(ctx); err != nil {
		t.Fatalf("partial batch failure must not abort remaining due pages: %v", err)
	}
	last, err := service.accounts.Get(ctx, lastID)
	if err != nil {
		t.Fatal(err)
	}
	if last.LastRefreshAt == nil {
		t.Fatal("account on the second due page was not refreshed")
	}
	if adapter.refreshCount.Load() != int64(credentialRefreshBatchSize+1) {
		t.Fatalf("refresh count = %d, want %d", adapter.refreshCount.Load(), credentialRefreshBatchSize+1)
	}
}
