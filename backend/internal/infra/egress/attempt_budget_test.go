package egress

import (
	"errors"
	"net/http"
	"testing"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
)

func TestLogicalBudgetIncludesConnectionRetriesAndReservedRecovery(t *testing.T) {
	budget := inferencedomain.NewAttemptBudget(2)
	ctx := WithPhysicalCallBudget(ledgerContext(), budget)
	permit, err := budget.Reserve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithPhysicalCallPermit(ctx, permit)
	client := &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) {
		return nil, errors.New("proxyconnect tcp: connection refused")
	}}
	lease := &Lease{client: client, proxyPool: true}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/responses", http.NoBody)
	_, err = lease.Do(request)
	permit.Release()
	if !errors.Is(err, inferencedomain.ErrAttemptBudget) || client.calls != 2 || len(PhysicalFacts(ctx)) != 2 || budget.Remaining() != 0 {
		t.Fatalf("err=%v calls=%d facts=%d remaining=%d", err, client.calls, len(PhysicalFacts(ctx)), budget.Remaining())
	}
	if _, ok := feedbackKind(domainegress.ScopeBuild, 0, err); ok {
		t.Fatal("budget rejection degraded network health")
	}
}

func TestTransportCannotUseAnotherLogicalRequestsReservation(t *testing.T) {
	budget := inferencedomain.NewAttemptBudget(1)
	other := inferencedomain.NewAttemptBudget(1)
	permit, err := other.Reserve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	ctx := WithPhysicalCallPermit(WithPhysicalCallBudget(ledgerContext(), budget), permit)
	if err = acquirePhysicalCallBudget(ctx); !errors.Is(err, inferencedomain.ErrAttemptBudget) {
		t.Fatalf("foreign permit consumed: %v", err)
	}
	if budget.Remaining() != 1 {
		t.Fatal("foreign permit charged current request")
	}
}
