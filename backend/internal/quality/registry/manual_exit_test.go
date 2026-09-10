package registry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestManualExitReleaseRevokesOwnersWithoutReleasingAccounts(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			r := newReconcileRegistry(t)
			ctx, now := context.Background(), time.Now().UTC()
			first, err := r.OpenInvestigation(ctx, 7, model.EpochKey{NodeID: 9}, now, `{"trigger":"original"}`)
			if err != nil {
				t.Fatal(err)
			}
			second, err := r.OpenInvestigation(ctx, 8, model.EpochKey{NodeID: 9}, now, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.SettleInvestigation(ctx, second, model.VerdictExitGuilty, `{"original_verdict":"exit"}`, now, true); err != nil {
				t.Fatal(err)
			}
			if fail {
				if err := r.DB().Exec("CREATE TRIGGER fail_manual_exit BEFORE UPDATE ON q_case_party BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			err = r.ReleaseCurrentExitAfterReview(ctx, 9, "operator verified recovered path")
			if (err != nil) != fail {
				t.Fatalf("release: %v", err)
			}
			allowed, err := r.ExitAllowed(ctx, 9)
			if err != nil || allowed == fail || r.ExitEligible(9) == fail {
				t.Fatalf("actual allowed=%v projected=%v %v", allowed, r.ExitEligible(9), err)
			}
			if r.AccountEligible(7) {
				t.Fatal("exit release also released account")
			}
			record, _, err := r.GetCase(ctx, first)
			if err != nil || strings.Contains(record.EvidenceJSON, "manual_exit_release") == fail {
				t.Fatalf("audit: %+v %v", record, err)
			}
			if fail {
				return
			}
			if id, err := r.OpenCaseForIncident(ctx, 7, model.EpochKey{NodeID: 9}); err != nil || id != first {
				t.Fatalf("release lost incident deduplication: %d %v", id, err)
			}
			if err := r.SettleInvestigation(ctx, first, model.VerdictExitGuilty, record.EvidenceJSON, now, true); err != nil {
				t.Fatal(err)
			}
			if allowed, err := r.ExitAllowed(ctx, 9); err != nil || !allowed {
				t.Fatalf("later verdict reinstated reviewed hold: %v %v", allowed, err)
			}
			// A new incident has a new owner and can still protect this exit.
			if _, err := r.OpenInvestigation(ctx, 10, model.EpochKey{NodeID: 9}, now, `{}`); err != nil {
				t.Fatal(err)
			}
			if allowed, err := r.ExitAllowed(ctx, 9); err != nil || allowed {
				t.Fatalf("review exempted future incidents: %v %v", allowed, err)
			}
		})
	}
}
