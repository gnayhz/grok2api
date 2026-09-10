package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Quota reset leaves the independently owned health restriction intact.
func TestResetQuotaStatePreservesPenaltyCooldown(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 2)
	ids := accountModelIDs(rows)
	until := time.Now().UTC().Add(24 * time.Hour)
	if err := database.db.Model(&accountModel{}).Where("id = ?", ids[0]).Updates(map[string]any{
		"cooldown_until": until, "failure_count": 3, "last_error": "upstream status 504",
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := repo.ResetQuotaState(ctx, account.ProviderBuild, ids); err != nil {
		t.Fatal(err)
	}

	var after accountModel
	if err := database.db.Where("id = ?", ids[0]).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.CooldownUntil == nil || !after.CooldownUntil.Equal(until) {
		t.Fatalf("cooldown_until = %v, want original deadline", after.CooldownUntil)
	}
	if after.FailureCount != 3 {
		t.Fatalf("failure_count = %d, want 3", after.FailureCount)
	}
	if after.LastError != "upstream status 504" {
		t.Fatalf("last_error = %q, want original reason", after.LastError)
	}
	// 健康账号不应产生无效写入副作用（列值保持原样）。
	var healthy accountModel
	if err := database.db.Where("id = ?", ids[1]).Take(&healthy).Error; err != nil {
		t.Fatal(err)
	}
	if healthy.FailureCount != 0 || healthy.LastError != "" || healthy.CooldownUntil != nil {
		t.Fatalf("healthy account mutated: %+v", healthy)
	}
}

// Both active and disabled accounts keep their independent health state.
func TestResetProviderQuotaStatePreservesPenaltyCooldown(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 2)
	ids := accountModelIDs(rows)
	until := time.Now().UTC().Add(24 * time.Hour)
	if err := database.db.Model(&accountModel{}).Where("id IN ?", ids).Updates(map[string]any{
		"cooldown_until": until, "failure_count": 1, "last_error": "upstream status 504",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Model(&accountModel{}).Where("id = ?", ids[1]).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}

	count, err := repo.ResetProviderQuotaState(ctx, account.ProviderBuild, true)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reset count = %d, want 1", count)
	}

	var active, disabled accountModel
	if err := database.db.Where("id = ?", ids[0]).Take(&active).Error; err != nil {
		t.Fatal(err)
	}
	if active.CooldownUntil == nil || !active.CooldownUntil.Equal(until) || active.FailureCount != 1 || active.LastError != "upstream status 504" {
		t.Fatalf("active health restriction changed: %+v", active)
	}
	if err := database.db.Where("id = ?", ids[1]).Take(&disabled).Error; err != nil {
		t.Fatal(err)
	}
	if disabled.CooldownUntil == nil || disabled.FailureCount != 1 {
		t.Fatalf("disabled account penalty should be untouched: %+v", disabled)
	}
}
