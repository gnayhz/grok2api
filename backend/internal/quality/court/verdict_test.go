package court

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"testing"
	"time"
)

func TestFilterIdentityGroupJurorsCountsOneSSOIdentityOnce(t *testing.T) {
	// This deliberately general graph tests the independence rule; real
	// Web/Build/Console links are one-to-one and contain at most one Build.
	bench := newBenchWithEvidenceConfig(t, evidence.DefaultConfig(), identityLinksStub{
		{AccountID: 100, RelatedAccountID: 7}, {AccountID: 100, RelatedAccountID: 8},
		{AccountID: 100, RelatedAccountID: 9}, {AccountID: 200, RelatedAccountID: 10},
		{AccountID: 200, RelatedAccountID: 11},
	})

	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newTestCourt(bench, cfg)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	jurors := service.filterIdentityGroupJurors(7, []uint64{8, 9, 10, 11, 1})
	if len(jurors) != 2 {
		t.Fatalf("same SSO identity must contribute one juror per group, got %v", jurors)
	}
	seen := map[uint64]bool{}
	for _, juror := range jurors {
		seen[juror] = true
	}
	if !seen[10] && !seen[11] {
		t.Fatalf("second SSO group should retain one juror, got %v", jurors)
	}
	if !seen[1] {
		t.Fatalf("unlinked juror should remain eligible, got %v", jurors)
	}
	if seen[8] || seen[9] {
		t.Fatalf("defendant identity group must be excluded entirely, got %v", jurors)
	}
}

type identityLinksStub []account.IdentityLink

func (links identityLinksStub) ListIdentityLinks(context.Context) ([]account.IdentityLink, error) {
	return links, nil
}
