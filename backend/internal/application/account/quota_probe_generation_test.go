package account

import (
	"context"
	"errors"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

type quotaProbeCompletionAdapter struct {
	*credentialRefreshAdapter
	complete func()
}

func (a *quotaProbeCompletionAdapter) GetBilling(context.Context, accountdomain.Credential) (accountdomain.Billing, error) {
	a.complete()
	return a.billing, a.billingErr
}

func TestQuotaProbeLateCompletionDoesNotUndoReset(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fails], func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			s, v, a := newCredentialRefreshTestService(t, now)
			a.billing = accountdomain.Billing{MonthlyLimit: 100, Used: 100, BillingPeriodEnd: now.Add(time.Hour).Format(time.RFC3339)}
			if fails {
				a.billingErr = errors.New("old billing failed")
			}
			if err := testsupport.Recovery(ctx, s.accounts, accountdomain.QuotaRecovery{AccountID: v.ID, Kind: accountdomain.QuotaRecoveryKindPaid, Status: accountdomain.QuotaRecoveryStatusProbing, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			s.providers = providerimpl.NewRegistry(&quotaProbeCompletionAdapter{credentialRefreshAdapter: a, complete: func() {
				if _, err := s.BatchResetQuotaState(ctx, []uint64{v.ID}); err != nil {
					t.Fatal(err)
				}
			}})
			v, err := s.accounts.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _ = s.ProbePaidQuota(ctx, v, v.QuotaRecoveryRef())
			if _, err := s.accounts.GetQuotaRecovery(ctx, v.ID); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("old paid probe completion undid reset: %v", err)
			}
		})
	}
}
