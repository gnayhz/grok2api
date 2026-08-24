package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// TestSelectorSkipsRiskFlaggedAccounts：rsc_denied 标记的账号保持 enabled，
// 但调度必须跳过；清空标记后恢复可选。
func TestSelectorSkipsRiskFlaggedAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-risk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	flagged, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "flagged", SourceKey: "flagged", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 200, MaxConcurrent: 1, RiskStatus: account.RiskStatusRSCDenied,
	})
	if err != nil {
		t.Fatal(err)
	}
	healthy, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "healthy", SourceKey: "healthy", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != healthy.ID {
		t.Fatalf("lease = %d, want risk-flagged account skipped in favor of %d", lease.Credential.ID, healthy.ID)
	}
	lease.Release()

	// 解除风控后应恢复可选（高优先级）。Update 在生产环境会经 invalidation
	// observer 触发同样的 Base 层失效；测试直接等价复放。
	cleared := flagged
	cleared.RiskStatus = ""
	if _, err := accounts.Update(ctx, cleared); err != nil {
		t.Fatal(err)
	}
	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild, AccountID: flagged.ID})
	lease, err = selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != flagged.ID {
		t.Fatalf("lease = %d, want un-flagged account %d selectable again", lease.Credential.ID, flagged.ID)
	}
	lease.Release()
}

// TestAcquirePinnedSkipsRiskFlagged：pinned 路径（previous_response_id 等固定
// 账号场景）同样必须跳过风控标记账号（D 审查缺口）。
func TestAcquirePinnedSkipsRiskFlagged(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "pinned-risk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	flagged, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "flagged", SourceKey: "flagged", EncryptedAccessToken: "enc", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 1, MaxConcurrent: 1, RiskStatus: account.RiskStatusRSCDenied,
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.acquirePinned(ctx, account.ProviderBuild, flagged.ID, 0, "grok-test", "", true, false, clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeAll}); err == nil {
		t.Fatal("pinned acquire of a risk-flagged account must fail, not serve the flagged identity")
	}
}

// TestSummaryCountsRiskFlagged：汇总的 available 排除风控标记账号，risk 计数包含它们。
func TestSummaryCountsRiskFlagged(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "summary-risk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "flagged", SourceKey: "flagged", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 1, MaxConcurrent: 1, RiskStatus: account.RiskStatusRSCDenied,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "healthy", SourceKey: "healthy", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := accounts.Summarize(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var summary *repository.AccountSummary
	for index := range rows {
		if rows[index].Provider == string(account.ProviderBuild) {
			summary = &rows[index]
		}
	}
	if summary == nil {
		t.Fatal("build summary row missing")
	}
	if summary.Available != 1 {
		t.Fatalf("available = %d, want 1 (risk-flagged excluded)", summary.Available)
	}
	if summary.RiskFlagged != 1 {
		t.Fatalf("risk_flagged = %d, want 1", summary.RiskFlagged)
	}

	if _, err := accounts.Get(ctx, 0); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("Get(0) error = %v, want ErrNotFound", err)
	}
}
