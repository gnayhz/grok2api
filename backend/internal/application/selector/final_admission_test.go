package selector

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

type admissionGate struct {
	blocked bool
	err     error
	checks  int
}

func (*admissionGate) AccountSchedulable(uint64) bool { return true }
func (g *admissionGate) CheckAccountAdmission(context.Context, uint64) (bool, error) {
	g.checks++
	return !g.blocked, g.err
}

func TestFinalAccountAdmissionDoesNotTrustCachedEligibility(t *testing.T) {
	limiter := memory.NewConcurrencyLimiter()
	s := NewSelector(nil, limiter, nil, nil, time.Minute, time.Second, time.Minute)
	gate := &admissionGate{blocked: true}
	s.SetQualityEligibility(gate)
	candidate := accountdomain.RoutingCandidate{Credential: accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild, Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1}}
	scope, _ := clientkeydomain.NormalizeAccountScope(clientkeydomain.AccountScope{})
	criteria := selectionCriteria{provider: accountdomain.ProviderBuild, accountScope: scope}
	lease, err := s.claimAccountSlotTracked(context.Background(), candidate, criteria, nil)
	if lease != nil || !errors.Is(err, errRoutingCredentialStale) || gate.checks != 1 {
		t.Fatalf("lease=%v err=%v checks=%d", lease, err, gate.checks)
	}
	if count, _ := limiter.Current(context.Background(), repository.AccountConcurrencyKey(7)); count != 0 {
		t.Fatal("rejected admission leaked capacity")
	}
	gate.err = errors.New("database unavailable")
	if lease, err = s.claimAccountSlotTracked(context.Background(), candidate, criteria, nil); lease != nil || err == nil {
		t.Fatalf("failed authority authorized account: lease=%v err=%v", lease, err)
	}
	gate.err = nil
	gate.blocked = false
	if lease, err = s.claimAccountSlotTracked(context.Background(), candidate, criteria, nil); err != nil || lease == nil {
		t.Fatalf("released restriction still denied: %v", err)
	}
	lease.Release()
	lease.Release()
}
