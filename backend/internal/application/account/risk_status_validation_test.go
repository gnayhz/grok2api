package account

import (
	"context"
	"errors"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestUpdateRiskStatusValidation 锁定 riskStatus 的白名单契约：
//   - "rsc_denied" 与 ""（清除）合法；
//   - 其他值一律 ErrInvalidFilter/invalidInput 拒绝；
//   - nil（不修改）保持原值。
func TestUpdateRiskStatusValidation(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	created, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "risk-validation", SourceKey: "risk-validation",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	flag := "rsc_denied"
	if _, err := service.Update(ctx, created.ID, UpdateInput{RiskStatus: &flag}); err != nil {
		t.Fatalf("rsc_denied must be accepted: %v", err)
	}
	view, err := service.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Credential.RiskStatus != accountdomain.RiskStatusRSCDenied {
		t.Fatalf("riskStatus = %q, want rsc_denied", view.Credential.RiskStatus)
	}

	cleared := ""
	if _, err := service.Update(ctx, created.ID, UpdateInput{RiskStatus: &cleared}); err != nil {
		t.Fatalf("empty clear must be accepted: %v", err)
	}
	view, err = service.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Credential.RiskStatus != "" {
		t.Fatalf("riskStatus = %q after clear, want empty", view.Credential.RiskStatus)
	}

	bogus := "bogus_status"
	if _, err := service.Update(ctx, created.ID, UpdateInput{RiskStatus: &bogus}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bogus riskStatus must be rejected with ErrInvalidInput, got %v", err)
	}
	view, err = service.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Credential.RiskStatus != "" {
		t.Fatalf("rejected write must not mutate stored value, got %q", view.Credential.RiskStatus)
	}
}
