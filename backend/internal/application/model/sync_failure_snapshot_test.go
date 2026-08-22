package model

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"encoding/base64"
)

// failingModelsAdapter 让全部账号的模型能力同步失败。
type failingModelsAdapter struct {
	*modelCapabilityAdapter
}

func (failingModelsAdapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	return nil, errors.New("upstream unavailable")
}

// TestSyncProgressReportsFailure 锁定进度快照的失败语义：恢复观众依赖
// SyncProgress().Err 判定失败并显示错误提示，而不是把失败的运行报告为成功。
func TestSyncProgressReportsFailure(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sync-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	if _, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "failing", SourceKey: "failing", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive}); err != nil {
		t.Fatal(err)
	}

	registry := provider.NewRegistry(failingModelsAdapter{modelCapabilityAdapter: &modelCapabilityAdapter{}})
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, nil)
	service := NewService(modelRepo, accountRepo, accountService, registry)

	if _, err := service.SyncObserved(ctx, nil); err == nil {
		t.Fatal("全部账号同步失败时 SyncObserved 应返回错误")
	}
	snapshot := service.SyncProgress()
	if snapshot.Active {
		t.Fatal("失败的运行不应保持 Active")
	}
	if snapshot.Err == nil {
		t.Fatal("失败的运行必须在进度快照中暴露 Err（恢复观众据此显示失败提示）")
	}
}
