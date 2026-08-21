package model

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// slowSyncAdapter 让每个账号的模型同步耗时可控，用于观察断开后运行态。
type slowSyncAdapter struct {
	modelCapabilityAdapter
	gate atomic.Bool
}

func (a *slowSyncAdapter) block() { for !a.gate.Load() { time.Sleep(time.Millisecond) } }
func (a *slowSyncAdapter) release() { a.gate.Store(true) }

// TestSyncObservedSurvivesWatcherDisconnect 锁定断线续跑语义：观察者（SSE
// 连接）提前取消后，脱钩的后台同步必须继续完成，进度快照对重连客户端
// 保持可查询（SyncProgress）。
func TestSyncObservedSurvivesWatcherDisconnect(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sync-detach.db"))
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
	if _, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "detached", SourceKey: "detached", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive}); err != nil {
		t.Fatal(err)
	}

	adapter := &slowSyncAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	service := NewService(modelRepo, accountRepo, accountService, registry)

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = service.SyncObserved(watchCtx, nil) // 观察者取消产生的返回值被忽略
	}()

	// 等待运行态。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if service.SyncProgress().Active {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !service.SyncProgress().Active {
		t.Fatal("同步未进入运行态")
	}

	// 模拟浏览器断开。
	cancelWatch()

	// 后台同步必须仍然完成。
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := service.SyncProgress()
		if !snapshot.Active {
			if snapshot.Err != nil {
				t.Fatalf("后台同步失败: %v", snapshot.Err)
			}
			if snapshot.Total == 0 || snapshot.Completed != snapshot.Total {
				t.Fatalf("同步未完成: %d/%d", snapshot.Completed, snapshot.Total)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("观察者 goroutine 未退出")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("断开后后台同步未在期限内完成")
}
