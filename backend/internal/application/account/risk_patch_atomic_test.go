package account

import (
	"context"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestPatchRiskStatusAtomicWithInvalidSibling：riskStatus 合法但组合字段非法
// 时整单 400，风险状态不得被部分写入（外部复核 9 的原子性回归）。
func TestPatchRiskStatusAtomicWithInvalidSibling(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	created, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "atomic", SourceKey: "atomic",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	cloudflare := "session=abc" // Build 账号拒绝 Cloudflare 字段
	if _, err := service.Update(ctx, created.ID, UpdateInput{RiskStatus: strPtr("rsc_denied"), CloudflareCookies: &cloudflare}); err == nil {
		t.Fatal("sibling validation must fail the whole update")
	}
	stored, err := accounts.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RiskStatus != "" {
		t.Fatalf("failed sibling must not leave riskStatus mutated: %q", stored.RiskStatus)
	}
	// 合法组合成功后两者都生效。
	if _, err := service.Update(ctx, created.ID, UpdateInput{RiskStatus: strPtr("rsc_denied")}); err != nil {
		t.Fatal(err)
	}
	stored, err = accounts.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RiskStatus != accountdomain.RiskStatusRSCDenied {
		t.Fatalf("valid solo riskStatus must persist: %q", stored.RiskStatus)
	}
}

func strPtr(v string) *string { return &v }
