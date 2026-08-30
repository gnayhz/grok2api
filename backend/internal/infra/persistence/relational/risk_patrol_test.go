package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestListPatrolDueSelection 锁定巡检到期语义：
//   - 已确认 denied/flagged 在 DeniedTTL 内不到期，过期后到期（自愈重探）；
//   - 未确认 denied 在 ErrorRetry 过期后到期；
//   - clean 超过 patrolInterval 到期；error 超过 errorRetryAfter 到期；
//   - 新 clean（未到期）、无 verdict、禁用账号不入选。
//
// 无 verdict 必须由降智事件 OnDegraded 做首次检测；巡检扫未检号会在
// 方法切换清 clean 后把整池打成永久 rsc_denied。
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
		streak  int
		checked time.Time
		wantDue bool
	}{
		// 已确认 denied/flagged 在 DeniedTTL(24h) 过期后 due,供巡检
		// 重探;TTL 内仍新鲜,不进 due(rsc_denied 排除调度,只有巡检
		// 能触发自愈,freshVerdict 过期本身不会重探)。
		{"denied-confirmed-old", "denied", 2, stale, true},
		{"denied-confirmed-fresh", "denied", 2, fresh, false},
		{"flagged-old", "flagged", 0, stale, true},
		{"flagged-fresh", "flagged", 0, fresh, false},
		// 未确认 denied(连击 1 < 2)在 ErrorRetry 过期后 due,供下一轮
		// 巡检补确认或被 clean 覆盖自愈(误判回归的新语义)。
		{"denied-unconfirmed-old", "denied", 1, stale, true},
		{"denied-unconfirmed-fresh", "denied", 1, fresh, false},
		{"clean-stale", "clean", 0, stale, true},
		{"clean-fresh", "clean", 0, fresh, false},
		{"error-stale", "error", 0, stale, true},
		{"error-fresh", "error", 0, fresh, false},
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
			AccountID: result.ID, Verdict: item.verdict, DeniedStreak: item.streak, Source: "rsc", CheckedAt: item.checked,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 无 verdict 的启用账号不得入选（首次检测走 OnDegraded）；禁用账号不入选。
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

	due, err := repo.ListPatrolDue(ctx, account.ProviderWeb, now.Add(-24*time.Hour), now.Add(-2*time.Hour), now.Add(-24*time.Hour), 2, 100)
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
	if got[neverChecked.ID] {
		t.Fatal("account without any verdict must not be patrol-due")
	}
	if got[disabled.ID] {
		t.Fatal("disabled account must never be patrol-due")
	}
}
