package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Late rejection of the pre-rotation material must preserve the replacement.
func TestCredentialRejectionPreservesConcurrentRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "account-refresh-preserve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)

	created := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "build", SourceKey: "preserve-1",
	})
	// 实际条件轮换安装新 token。
	newExpiry := time.Now().Add(2 * time.Hour).UTC()
	if _, err := rotateOAuthFixture(repo, ctx, created.ID, "primary-new", "refresh-new", newExpiry, 0); err != nil {
		t.Fatal(err)
	}

	// 刷新前的旧材料返回迟到认证拒绝。
	stale := created
	stale.AuthStatus = account.AuthStatusReauthRequired
	stale.LastError = "concurrent flag write"
	if _, err := repo.ApplyCredential(ctx, stale.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRejected, Reason: "old rejected"}); err != nil {
		t.Fatal(err)
	}

	after, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EncryptedRefreshToken != "refresh-new" {
		t.Fatalf("stale credential rejection rolled back rotated refresh token: %q", after.EncryptedRefreshToken)
	}
	if after.AuthStatus != account.AuthStatusActive || after.AuthError != "" || after.EncryptedAccessToken != "primary-new" {
		t.Fatalf("stale rejection changed current credential: status=%q error=%q", after.AuthStatus, after.LastError)
	}
	_ = newExpiry
}

// 对照面:重新导入(upsert 已有账号)携带的新 refresh token 必须原样落库——
// 代际条件只限制结果事件，不得吞掉明确导入的新凭据。
func TestUpsertReimportStillWritesNewRefreshToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "account-reimport-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)

	created := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "build", SourceKey: "reimport-1",
	})
	if _, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "build", SourceKey: "reimport-1",
		EncryptedAccessToken: "access-reimported", EncryptedRefreshToken: "refresh-reimported",
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		Enabled:   true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EncryptedRefreshToken != "refresh-reimported" {
		t.Fatalf("re-imported refresh token not persisted: %q", after.EncryptedRefreshToken)
	}
	if after.EncryptedAccessToken != "access-reimported" {
		t.Fatalf("re-imported access token not persisted: %q", after.EncryptedAccessToken)
	}
}
