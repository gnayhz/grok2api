package model

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestSyncObservedSurvivesWatcherDisconnect 锁定断线续跑语义：观察者（SSE
// 连接）提前取消后，脱钩的后台同步必须继续完成，进度快照对重连客户端
// 保持可查询（SyncProgress）。用门控 adapter 阻塞 ListModels，确保运行态
// 窗口确定可观察——无门控时单账号同步可在毫秒内完成，Active 窗口比轮询
// 间隔还短（慢 CI 上必然漏检，曾致流水线闪断失败）。
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
	created, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "detached", SourceKey: "detached", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}

	// 门控 adapter：ListModels 阻塞在 entered/release 握手上，直到测试放行。
	adapter := &modelCapabilityAdapter{
		models:  map[uint64][]string{created.ID: {"grok-detached"}},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	service := NewService(modelRepo, accountRepo, accountService, registry)

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = service.SyncObserved(watchCtx, nil)
	}()

	// 等待同步真正进入账号执行阶段：ListModels 已挂起 → Active 必为 true。
	select {
	case <-adapter.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("同步未进入账号执行阶段")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if service.SyncProgress().Active {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !service.SyncProgress().Active {
		t.Fatal("同步已挂起在账号执行阶段但快照未标记 Active")
	}

	// 模拟浏览器断开（同步仍被门控挂起，必然处于运行态）。
	cancelWatch()

	// 放行被挂起的同步：后台运行必须继续到完成。
	close(adapter.release)
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
