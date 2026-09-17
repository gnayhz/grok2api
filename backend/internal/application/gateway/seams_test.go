package gateway

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// stubAccountEligibility 可编程账号资格谓词。
type stubAccountEligibility struct {
	ineligible map[uint64]bool
}

func (s stubAccountEligibility) AccountSchedulable(accountID uint64) bool {
	return !s.ineligible[accountID]
}

func newSeamSelectorDB(t *testing.T, names ...string) (*relational.Database, []account.Credential) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sel-seam.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credentials := make([]account.Credential, 0, len(names))
	for index, name := range names {
		credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100 - index, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		credentials = append(credentials, credential)
	}
	return database, credentials
}

// TestSelectorEligibilitySeamSkipsIneligible 锚定 D3-1:资格谓词缝隙
// 把质量层判定的不可调度账号移出生产调度;缝隙未注入时行为不变
// (既有全套 sel 测试即零变化证明)。
func TestSelectorEligibilitySeamSkipsIneligible(t *testing.T) {
	ctx := context.Background()
	database, credentials := newSeamSelectorDB(t, "seam-a", "seam-b")
	accounts := relational.NewAccountRepository(database)
	sel := selector.NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	sel.SetQualityEligibility(stubAccountEligibility{ineligible: map[uint64]bool{credentials[0].ID: true}})
	session, sessionErr := sel.BeginSelectionSessionForKey(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true, clientkeydomain.AccountScope{})
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	lease, err := session.Acquire(ctx, map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != credentials[1].ID {
		t.Fatalf("缝隙必须移出不可调度账号: lease=%d, want %d", lease.Credential.ID, credentials[1].ID)
	}
	lease.Release()
	// 全部不可调度 → 无账号可选。
	sel.SetQualityEligibility(stubAccountEligibility{ineligible: map[uint64]bool{credentials[0].ID: true, credentials[1].ID: true}})
	if session, sessionErr := sel.BeginSelectionSessionForKey(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true, clientkeydomain.AccountScope{}); sessionErr == nil {
		if _, err := session.Acquire(ctx, map[uint64]bool{}, true); err == nil {
			t.Fatal("全部不可调度时必须无账号可选")
		}
	}
}

// TestSelectorEligibilityPinnedAndProbeBypass 锚定 B2 生产/探孔双通道:
// 生产钉住尊重质量资格;取证探测(AcquirePinnedForQualityProbe)放行。
func TestSelectorEligibilityPinnedAndProbeBypass(t *testing.T) {
	ctx := context.Background()
	database, credentials := newSeamSelectorDB(t, "seam-pinned")
	accounts := relational.NewAccountRepository(database)
	sel := selector.NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	sel.SetQualityEligibility(stubAccountEligibility{ineligible: map[uint64]bool{credentials[0].ID: true}})
	if _, err := sel.AcquirePinnedForKey(ctx, account.ProviderBuild, credentials[0].ID, 0, "grok-test", "", true, clientkeydomain.AccountScope{}); err == nil {
		t.Fatal("生产钉住必须尊重质量资格")
	}
	scope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeAll}
	lease, err := sel.AcquirePinnedForQualityProbe(ctx, account.ProviderBuild, credentials[0].ID, 0, "grok-test", "", scope)
	if err != nil {
		t.Fatalf("取证探测不受资格限制(B2 双通道): %v", err)
	}
	lease.Release()
}
