package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// 适用边界：多账号循环测试（流式/非流式 hold、fail-closed）。单账号 +
// keyRepo 的 fail-open 变体与 Web 供应商旁路测试各自保留内联播种——
// 它们的凭据形状/键需求不同，强行套用反而增加耦合。
// 适用边界：多账号循环测试（流式/非流式 hold、fail-closed）。单账号 +
// keyRepo 的 fail-open 变体与 Web 供应商旁路测试各自保留内联播种——
// 它们的凭据形状/键需求不同，强行套用反而增加耦合。
// newGuardLoopDatabase 建库并按 names 播种 Build 凭证（priority 递减保证
// 调度顺序与切片顺序一致），为每个凭证开通 model 能力。守卫循环类测试
// （流式/非流式/兜底失败/Web 旁路）共用的最小脚手架。
func newGuardLoopDatabase(t *testing.T, model string, names ...string) (*relational.Database, []accountdomain.Credential) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "guard-loop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := relational.NewModelRepository(database).UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{model}); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	credentials := make([]accountdomain.Credential, 0, len(names))
	for index, name := range names {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			EncryptedRefreshToken: "refresh-" + name, ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	for _, credential := range credentials {
		if err := relational.NewModelRepository(database).ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	return database, credentials
}

// newGuardLoopService 完成守卫循环 e2e 的服务装配：播种库 + 适配器注册表 +
// 账号/选择器/网关服务 + 守卫全开（fail-closed、预算 2）。四个非流式
// e2e（chat/messages × deliver/fail-closed，rounds 43/44/63/75）共享此前
// 各自的 ~18 行装配，现收敛为一行。
func newGuardLoopService(t *testing.T, adapter *scriptedBuildAdapter, names ...string) (*Service, []accountdomain.Credential) {
	t.Helper()
	database, credentials := newGuardLoopDatabase(t, "grok-4.6", names...)
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)
	service.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, MaxAttempts: 2, OnExhausted: qualityRetryFailClosed})
	return service, credentials
}
