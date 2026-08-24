package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// 全实体 Update(MarkReauthRequired/SetAccountEnabled/管理端编辑的持久化路径)
// 不得回滚并发 OAuth 刷新已轮换的 refresh token——上游作废旧值后, 回滚即
// 账号永久失效。刷新生命周期列由 preserveConcurrentRefreshWrites 保护。
func TestUpdatePreservesConcurrentRefreshTokenRotation(t *testing.T) {
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
	// 模拟并发刷新:UpdateTokens(唯一定向写方)写入已轮换的新 token。
	newExpiry := time.Now().Add(2 * time.Hour).UTC()
	if _, err := repo.UpdateTokens(ctx, created.ID, "primary-new", "refresh-new", newExpiry, 0); err != nil {
		t.Fatal(err)
	}

	// 用刷新发生前的旧快照做一次全实体保存(风控标记/停用/管理端编辑路径)。
	stale := created
	stale.AuthStatus = account.AuthStatusReauthRequired
	stale.LastError = "concurrent flag write"
	if _, err := repo.Update(ctx, stale); err != nil {
		t.Fatal(err)
	}

	after, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EncryptedRefreshToken != "refresh-new" {
		t.Fatalf("stale full-entity save rolled back rotated refresh token: %q", after.EncryptedRefreshToken)
	}
	if after.AuthStatus != account.AuthStatusReauthRequired || after.LastError != "concurrent flag write" {
		t.Fatalf("intended status write lost: status=%q error=%q", after.AuthStatus, after.LastError)
	}
	_ = newExpiry
}

// 对照面:重新导入(upsert 已有账号)携带的新 refresh token 必须原样落库——
// 保留逻辑只应保护 Update 路径, 不得吞掉导入的新凭据。
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
