package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestUpgradePreservesExistingQualityState(t *testing.T) {
	ctx := context.Background()
	opts := qualityregistry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")}
	reg, err := qualityregistry.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if reg != nil {
			_ = reg.Close()
		}
	})
	caseID, err := reg.CreateCase(ctx, time.Now().UTC(), `{"fixture":"preserved case"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.TransitionAccount(ctx, model.AccountTransitionRequest{
		AccountID: 7, To: model.AccountRemanded, CaseID: caseID,
	}); err != nil {
		t.Fatal(err)
	}
	if reg.AccountEligible(7) {
		t.Fatal("precondition: remanded account must be ineligible")
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	reg, err = qualityregistry.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if reg.AccountEligible(7) || reg.AccountState(7).State != model.AccountRemanded {
		t.Fatal("reopening must reconstruct the committed hold cache")
	}
	caseRow, found, err := reg.GetCase(ctx, caseID)
	if err != nil || !found || caseRow.Status != model.CaseInvestigating {
		t.Fatalf("case lost on reopen: %+v found=%v err=%v", caseRow, found, err)
	}
	evidence, found, err := reg.CaseEvidence(ctx, caseID)
	if err != nil || !found || evidence != `{"fixture":"preserved case"}` {
		t.Fatalf("case evidence lost: found=%v err=%v", found, err)
	}
}
