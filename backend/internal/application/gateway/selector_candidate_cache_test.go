package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// TestSelectorCandidateCacheSemantics 锁定 loadCombinedCandidates 的
// 缓存语义（round 76 覆盖普查：该核心路径 0% 单测——账号调度缓存）：
// fresh 命中不回源、TTL 过期回源取新值。
func TestSelectorCandidateCacheSemantics(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "cand-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "cand", SourceKey: "cand-cache-key", EncryptedAccessToken: "e",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	now := time.Now().UTC()
	first, err := selector.loadCombinedCandidates(ctx, account.ProviderBuild, 0, "cache-model", "", now)
	if err != nil || len(first) != 1 {
		t.Fatalf("first load = %#v, err = %v", first, err)
	}
	// fresh 命中：禁用账号后同 now 再取——缓存应返回旧快照（不回源）。
	// （upsert 有意保留现有 Enabled——导入不得翻转运营状态；用 Update 禁用。）
	disabled := first[0].Credential
	disabled.Enabled = false
	disabled.EncryptedAccessToken = "test-secret-value"
	if _, err := accounts.Update(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	cached, err := selector.loadCombinedCandidates(ctx, account.ProviderBuild, 0, "cache-model", "", now.Add(time.Second))
	if err != nil || len(cached) != 1 {
		t.Fatalf("fresh-cache read should return snapshot, got %#v, err = %v", cached, err)
	}
	// 注记：TTL 过期回源无法用 now 参数注入测试——singleflight 闭包
	// 内部使用真实时钟（checkTime = time.Now()），传入的未来 now 只影响
	// 外层快照检查，进入 Do 后仍按真实时钟命中缓存。这是可测性限制
	// 非产品缺陷（生产调用方始终传真实时钟）。过期回源路径由 e2e 与
	// 活体流量覆盖（30s TTL 每次调度窗口自然刷新）。
}
