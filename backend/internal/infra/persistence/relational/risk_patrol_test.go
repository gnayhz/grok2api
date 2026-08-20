package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestListPatrolDueSelection 锁定巡检到期语义：
//   - denied/flagged 永不到期（不重查，与“风控永不恢复”一致）；
//   - clean 超过 patrolInterval 到期；error 超过 errorRetryAfter 到期；
//   - 新 clean（未到期）与禁用账号不入选。
func TestListPatrolDueSelection(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "patrol.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewRiskRepository(database)
	accountRepo := NewAccountRepository(database)

	now := time.Now().UTC()
	stale := now.Add(-48 * time.Hour)
	fresh := now.Add(-time.Hour)
	seed := []struct {
		name    string
		verdict string
		checked time.Time
		wantDue bool
	}{
		{"denied-old", "denied", stale, false},
		{"flagged-old", "flagged", stale, false},
		{"clean-stale", "clean", stale, true},
		{"clean-fresh", "clean", fresh, false},
		{"error-stale", "error", stale, true},
		{"error-fresh", "error", fresh, false},
	}
	accountIDs := make(map[string]uint64)
	for _, item := range seed {
		result, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, Name: item.name, SourceKey: item.name, EncryptedAccessToken: "enc",
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		accountIDs[item.name] = result.ID
		if err := repo.SaveRiskVerdict(ctx, AccountRiskVerdict{
			AccountID: result.ID, Verdict: item.verdict, Source: "rsc", CheckedAt: item.checked,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 无 verdict 的启用账号应入选（首轮覆盖）；禁用账号不入选。
	neverChecked, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, Name: "never", SourceKey: "never", EncryptedAccessToken: "enc",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 生产禁用路径走 Update（UpsertByIdentity 创建时强制 enabled=true）。
	disabled, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, Name: "off", SourceKey: "off", EncryptedAccessToken: "enc",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled.Enabled = false
	if _, err := accountRepo.Update(ctx, disabled); err != nil {
		t.Fatal(err)
	}

	due, err := repo.ListPatrolDue(ctx, account.ProviderWeb, now.Add(-24*time.Hour), now.Add(-2*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[uint64]bool, len(due))
	for _, id := range due {
		got[id] = true
	}
	for _, item := range seed {
		if item.wantDue && !got[accountIDs[item.name]] {
			t.Fatalf("%s should be patrol-due", item.name)
		}
		if !item.wantDue && got[accountIDs[item.name]] {
			t.Fatalf("%s must not be patrol-due", item.name)
		}
	}
	if !got[neverChecked.ID] {
		t.Fatal("account without any verdict must be patrol-due on the first sweep")
	}
	if got[disabled.ID] {
		t.Fatal("disabled account must never be patrol-due")
	}
}
