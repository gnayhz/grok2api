package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestExitObservationFencesOlderReplicaAndRestart(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			opts := Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "shared.db")}
			if driver == "postgres" {
				opts, _ = isolatedQualityPostgres(t)
			}
			open := func() *Registry {
				r, err := Open(ctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
				return r
			}
			a, b := open(), open()
			if _, _, _, err := a.ObserveExitIP(ctx, 5, "192.0.2.1", 1); err != nil {
				t.Fatal(err)
			}
			if _, epoch, _, err := b.ObserveExitIP(ctx, 5, "192.0.2.2", 2); err != nil || epoch != 1 {
				t.Fatalf("new IP: %d %v", epoch, err)
			}
			id, err := b.OpenInvestigation(ctx, 9, model.EpochKey{NodeID: 5, Epoch: 1}, time.Now(), `{}`)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.SettleInvestigation(ctx, id, model.VerdictExitGuilty, `{}`, time.Now(), true); err != nil {
				t.Fatal(err)
			}
			for _, r := range []*Registry{a, open()} {
				for _, version := range []uint64{1, 2} {
					old, current, released, err := r.ObserveExitIP(ctx, 5, "192.0.2.1", version)
					if err != nil || old != 1 || current != 1 || len(released) != 0 {
						t.Fatalf("old observation changed state: %d %d %v %v", old, current, released, err)
					}
					if allowed, err := r.ExitAllowed(ctx, 5); err != nil || allowed {
						t.Fatalf("new ban lost: %v %v", allowed, err)
					}
				}
			}
			// Same IP observations must still advance the version fence.
			if _, _, _, err := a.ObserveExitIP(ctx, 5, "192.0.2.2", 4); err != nil {
				t.Fatal(err)
			}
			if _, epoch, _, err := b.ObserveExitIP(ctx, 5, "192.0.2.3", 3); err != nil || epoch != 1 {
				t.Fatalf("same-IP fence lost: %d %v", epoch, err)
			}
			if _, epoch, released, err := b.ObserveExitIP(ctx, 5, "192.0.2.3", 5); err != nil || epoch != 2 || len(released) != 1 {
				t.Fatalf("real new IP failed: %d %v %v", epoch, released, err)
			}
			if allowed, err := a.ExitAllowed(ctx, 5); err != nil || !allowed {
				t.Fatalf("new IP blocked: %v %v", allowed, err)
			}
			parties, err := a.ListParties(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range parties {
				if p.Kind == model.PartyExit && p.Disposition != model.DispositionWithdrawn {
					t.Fatalf("old sentence not withdrawn atomically: %+v", p)
				}
			}
		})
	}
}

func TestExitObservationFailureRollsBackFenceAndDisposition(t *testing.T) {
	r := openTestRegistry(t)
	ctx := context.Background()
	if _, _, _, err := r.ObserveExitIP(ctx, 5, "192.0.2.1", 1); err != nil {
		t.Fatal(err)
	}
	id, err := r.OpenInvestigation(ctx, 9, model.EpochKey{NodeID: 5}, time.Now(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictExitGuilty, `{}`, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if err := r.DB().Exec(`CREATE TRIGGER reject_observation BEFORE UPDATE OF observation_revision ON q_node_epoch BEGIN SELECT RAISE(ABORT,'injected observation failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.ObserveExitIP(ctx, 5, "192.0.2.2", 2); err == nil {
		t.Fatal("write failure hidden")
	}
	if r.CurrentEpoch(5) != 0 || r.ExitEligible(5) {
		t.Fatal("partial state published")
	}
	parties, err := r.ListParties(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parties {
		if p.Kind == model.PartyExit && p.Disposition != model.DispositionSentenced {
			t.Fatal("partial withdrawal committed")
		}
	}
	if err := r.DB().Exec("DROP TRIGGER reject_observation").Error; err != nil {
		t.Fatal(err)
	}
	if _, epoch, _, err := r.ObserveExitIP(ctx, 5, "192.0.2.2", 2); err != nil || epoch != 1 {
		t.Fatalf("same version not retryable: %d %v", epoch, err)
	}
}

func TestReconcileOldSentencesPreservesCurrentHoldsAndVerdicts(t *testing.T) {
	r := openTestRegistry(t)
	ctx := context.Background()
	oldID, err := r.OpenInvestigation(ctx, 9, model.EpochKey{NodeID: 5}, time.Now(), `{"retained":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, oldID, model.VerdictExitGuilty, `{"retained":true}`, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.AdvanceEpoch(ctx, 5, "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	// Emulate the legacy omission after a completed epoch change.
	if err := r.DB().Model(&qCasePartyModel{}).Where("case_id=? AND kind='exit'", oldID).Update("disposition", "sentenced").Error; err != nil {
		t.Fatal(err)
	}
	currentID, err := r.OpenInvestigation(ctx, 10, model.EpochKey{NodeID: 5, Epoch: 1}, time.Now(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReconcileStaleExitParties(ctx); err != nil || n != 1 {
		t.Fatalf("legacy reconciliation: %d %v", n, err)
	}
	if allowed, err := r.ExitAllowed(ctx, 5); err != nil || allowed {
		t.Fatalf("current hold lost: %v %v", allowed, err)
	}
	record, _, err := r.GetCase(ctx, oldID)
	if err != nil || record.Verdict != model.VerdictExitGuilty || record.EvidenceJSON != `{"retained":true}` {
		t.Fatalf("history changed: %+v %v", record, err)
	}
	parties, err := r.ListParties(ctx, currentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parties {
		if p.Disposition != model.DispositionRemanded {
			t.Fatalf("current party released: %+v", p)
		}
	}
	if n, err := r.ReconcileStaleExitParties(ctx); err != nil || n != 0 {
		t.Fatalf("reconcile not idempotent: %d %v", n, err)
	}
}
