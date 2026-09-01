package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// TestSelectorQuotaExhaustionInvalidatesBaseLayerOnly 锁定配额耗尽路径的
// 失效粒度:base 层失效、overlay 层保留。
//
// 配额耗尽(markFreeQuotaExhaustedAt / MarkPaymentQuotaExhausted)曾调用
// invalidateCandidates(provider)——base+overlay 两层全清。配额恢复状态由
// base 层承载:overlay 全清会把变更无关的 (model,quotaMode) 键全部卷入
// overlay SQL 重查(配额重置窗口=重载风暴),而 overlay 数据并未变化。
//
// 同时锁定语义契约(见 TestSpendingLimitBlockedMarksQuotaRecovery):
// assembled 快照必须被清理以促发 DB 重载,耗尽账号以「带恢复状态」的形态
// 回到候选池——选号因此返回 quota-exhausted(429/Retry-After)而不是
// no-accounts(503)。定向 evict(让账号从池中消失)会破坏该契约。
//
// 测试装配不挂仓储失效 observer,缓存变化只来自 Selector 自身。
func TestSelectorQuotaExhaustionInvalidatesBaseLayerOnly(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-layer-invalidate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)

	exhausted, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build-a", SourceKey: "build-a", EncryptedAccessToken: "encrypted-a",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build-b", SourceKey: "build-b", EncryptedAccessToken: "encrypted-b",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	const upstreamModel = "grok-quota-layer-model"
	cacheKey := candidateCacheKey{provider: account.ProviderBuild, modelRouteID: 0, upstreamModel: upstreamModel, quotaMode: ""}
	overlayKey := routingOverlayCacheKey{provider: account.ProviderBuild, modelRouteID: 0, upstreamModel: upstreamModel}

	warmup := func() {
		t.Helper()
		if _, err := selector.loadCandidates(ctx, account.ProviderBuild, 0, upstreamModel, "", time.Now().UTC()); err != nil {
			t.Fatalf("loadCandidates: %v", err)
		}
	}
	warmup()
	selector.candidateMu.Lock()
	overlayVersionBefore := selector.overlayProviderVersion[account.ProviderBuild]
	if _, ok := selector.routingOverlays[overlayKey]; !ok {
		selector.candidateMu.Unlock()
		t.Fatal("warmup failed: overlay snapshot missing")
	}
	if _, ok := selector.candidates[cacheKey]; !ok {
		selector.candidateMu.Unlock()
		t.Fatal("warmup failed: assembled snapshot missing")
	}
	selector.candidateMu.Unlock()

	selector.MarkFreeQuotaExhausted(ctx, exhausted, 1, 1)

	selector.candidateMu.Lock()
	_, assembledCached := selector.candidates[cacheKey]
	_, overlayCached := selector.routingOverlays[overlayKey]
	overlayVersionAfter := selector.overlayProviderVersion[account.ProviderBuild]
	selector.candidateMu.Unlock()
	if assembledCached {
		t.Fatal("assembled snapshot survived quota exhaustion; fresh recovery state would never be observed (429 quota semantics lost)")
	}
	if !overlayCached {
		t.Fatal("overlay cache fully invalidated; quota exhaustion must not force overlay reloads for unrelated model keys")
	}
	if overlayVersionAfter != overlayVersionBefore {
		t.Fatalf("overlay version bumped %d -> %d; full-provider overlay invalidation regression", overlayVersionBefore, overlayVersionAfter)
	}

	// 重载后:耗尽账号必须以「带恢复状态」形态回到候选(429 语义),
	// 另一账号不受影响。
	reloaded, err := selector.loadCandidates(ctx, account.ProviderBuild, 0, upstreamModel, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var sawExhausted, sawHealthy bool
	for _, candidate := range reloaded {
		switch candidate.Credential.ID {
		case exhausted.ID:
			if candidate.QuotaRecovery == nil || candidate.QuotaRecovery.Status != account.QuotaRecoveryStatusExhausted {
				t.Fatalf("exhausted account reloaded without recovery state: %#v", candidate.QuotaRecovery)
			}
			sawExhausted = true
		default:
			sawHealthy = true
		}
	}
	if !sawExhausted || !sawHealthy {
		t.Fatalf("reloaded candidates missing accounts: exhausted=%v healthy=%v", sawExhausted, sawHealthy)
	}

	// 端到端语义:单账号池耗尽后,选号必须给出 quota-exhausted(而非 no-accounts)。
	soloDB, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-layer-solo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer soloDB.Close()
	if err := soloDB.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	soloRepo := relational.NewAccountRepository(soloDB)
	soloCredential, _, err := soloRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "solo", SourceKey: "solo", EncryptedAccessToken: "encrypted-solo",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	solo := NewSelector(soloRepo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	solo.MarkFreeQuotaExhausted(ctx, soloCredential, 1, 1)
	_, soloErr := solo.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-solo-model", "", "", map[uint64]bool{}, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(soloErr, &unavailable) {
		t.Fatalf("expected selection unavailable, got %v", soloErr)
	}
	if unavailable.Reason != SelectionQuotaExhausted {
		t.Fatalf("selection reason = %s, want %s", unavailable.Reason, SelectionQuotaExhausted)
	}
}
