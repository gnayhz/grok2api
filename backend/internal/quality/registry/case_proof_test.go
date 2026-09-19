package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestRecentCaseProofsExcludeSingleMeasurementsBeforeLimit(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			opts, _ := resourceCheckDatabase(t, driver)
			r, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			now := time.Now().UTC()
			rows := []qProbeTaskModel{
				{ID: 1, Direction: "case_proof", CaseID: 81, CreatedAt: now.Add(time.Second)},
				{ID: 2, Direction: "case_proof", CaseID: 82, CreatedAt: now},
				{ID: 3, Direction: "case_proof", CaseID: 83, CreatedAt: now.Add(time.Second)},
				{ID: 4, Direction: "account_differential", CaseID: 84, CreatedAt: now.Add(time.Minute)},
				{ID: 5, Direction: "exit_jury", CaseID: 84, CreatedAt: now.Add(time.Minute)},
				{ID: 6, Direction: "resource_check", CreatedAt: now.Add(time.Minute)},
			}
			if err := r.db.Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
			store := NewProbeTaskStore(r)
			views, err := store.ListProbeTasks(ctx, 2)
			if err != nil || len(views) != 2 || views[0].ID != 3 || views[1].ID != 1 {
				t.Fatalf("recent proofs must filter before limiting and sort by time/id: %+v, %v", views, err)
			}
			all, err := store.ListProbeTasks(ctx, 0)
			if err != nil || len(all) != 3 {
				t.Fatalf("default feed: %+v, %v", all, err)
			}
			var count int64
			if err := r.db.Model(&qProbeTaskModel{}).Count(&count).Error; err != nil || count != int64(len(rows)) {
				t.Fatalf("listing must not delete evidence: %d, %v", count, err)
			}
		})
	}
}

func TestCaseProofUpgradePreservesHistoryAndBothHolders(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			opts, db := resourceCheckDatabase(t, driver)
			if err := db.AutoMigrate(&qCaseModel{}); err != nil {
				t.Fatal(err)
			}
			if driver == "postgres" {
				for _, pair := range [][2]string{{"chk_q_case_status", "status IN ('investigating','account_guilty','exit_guilty','dismissed')"}, {"chk_q_case_verdict", "verdict IN ('','account_guilty','exit_guilty','insufficient','dismissed')"}} {
					if err := db.Exec("ALTER TABLE q_case DROP CONSTRAINT " + pair[0]).Error; err != nil {
						t.Fatal(err)
					}
					if err := db.Exec("ALTER TABLE q_case ADD CONSTRAINT " + pair[0] + " CHECK (" + pair[1] + ")").Error; err != nil {
						t.Fatal(err)
					}
				}
			} else {
				var ddl string
				if err := db.Raw("SELECT sql FROM sqlite_master WHERE name='q_case'").Scan(&ddl).Error; err != nil {
					t.Fatal(err)
				}
				ddl = strings.ReplaceAll(ddl, ",'both_guilty'", "")
				if err := db.Exec("DROP TABLE q_case").Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Exec(ddl).Error; err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			old := qCaseModel{Status: "account_guilty", Verdict: "account_guilty", EvidenceJSON: `{"synthetic":"retained legacy evidence"}`, OpenedAt: now, UpdatedAt: now}
			if err := db.Create(&old).Error; err != nil {
				t.Fatal(err)
			}
			for range 2 {
				r, err := Open(ctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				got, found, err := r.GetCase(ctx, old.ID)
				if err != nil || !found || got.EvidenceJSON != old.EvidenceJSON || string(got.Verdict) != old.Verdict {
					t.Fatal("history lost", got, err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			}
			r, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err := r.RecordExitIdentity(ctx, 901, model.ExitIdentity{IPv4: "192.0.2.91"}); err != nil {
				t.Fatal(err)
			}
			first, err := r.OpenInvestigation(ctx, 801, model.EpochKey{NodeID: 901}, now, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.SettleInvestigation(ctx, first, model.VerdictBothGuilty, `{}`, now, true); err != nil {
				t.Fatal(err)
			}
			second, err := r.OpenInvestigation(ctx, 801, model.EpochKey{NodeID: 901}, now.Add(time.Second), `{}`)
			if err != nil || second == first {
				t.Fatal(second, err)
			}
			if err := r.SettleInvestigation(ctx, second, model.VerdictBothGuilty, `{}`, now, true); err != nil {
				t.Fatal(err)
			}
			if err := r.SettleInvestigation(ctx, second, model.VerdictInsufficient, `{}`, now, true, true); err != nil {
				t.Fatal(err)
			}
			if r.AccountEligible(801) || r.ExitEligible(901) {
				t.Fatal("another case's dual restrictions were released")
			}
			if err := r.SettleInvestigation(ctx, first, model.VerdictInsufficient, `{}`, now, true, true); err != nil {
				t.Fatal(err)
			}
			if !r.AccountEligible(801) || !r.ExitEligible(901) {
				t.Fatal("last holder was not released")
			}
		})
	}
}

func TestCaseProofOwnerBudgetCancellationAndNoManualProjection(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			opts, _ := resourceCheckDatabase(t, driver)
			r, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			now := time.Now().UTC()
			spec := resourceTask("account", 801).Experiment
			spec.ResourceCheck.Targets = []model.ResourceTarget{{Kind: "account", ResourceID: 801}, {Kind: "node", ResourceID: 901}}
			spec.ResourceCheck.MaxCalls = 6
			spec.ResourceCheck.DeadlineAt = now.Add(time.Minute)
			raw, _ := json.Marshal(map[string]any{"policy": map[string]any{"version": model.CaseProofVersion, "experiment": spec}})
			id, err := r.OpenInvestigation(ctx, 801, model.EpochKey{NodeID: 901}, now, string(raw))
			if err != nil {
				t.Fatal(err)
			}
			a, b := NewProbeTaskStore(r), NewProbeTaskStore(r)
			task := model.ProbeTask{CaseID: id, Direction: model.ProbeCaseProof, DefendantAccountID: 801, DefendantNodeID: 901}
			taskID, err := a.CreateProbeTask(ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			if duplicate, err := b.CreateProbeTask(ctx, task); err != nil || duplicate != taskID {
				t.Fatal("duplicate task", duplicate, err)
			}
			claimed, err := a.ClaimPendingProbeTasks(ctx, 1)
			if err != nil || len(claimed) != 1 {
				t.Fatal(claimed, err)
			}
			proof := model.ResourceCheckReport{Version: model.ResourceCheckVersion, Revision: 1, Calls: 1, MaxCalls: 6}
			if err := b.SaveResourceCheckProgress(ctx, taskID, proof); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatal("foreign owner wrote progress", err)
			}
			if err := a.SaveResourceCheckProgress(ctx, taskID, proof); err != nil {
				t.Fatal(err)
			}
			if manual, err := a.ListResourceChecks(ctx, "account", []uint64{801}); err != nil || len(manual) != 0 {
				t.Fatal("case proof leaked into manual history", manual, err)
			}
			if err := r.SettleInvestigation(ctx, id, model.VerdictInsufficient, `{}`, now, true); err != nil {
				t.Fatal(err)
			}
			proof.Revision++
			if err := a.SaveResourceCheckProgress(ctx, taskID, proof); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatal("late progress accepted", err)
			}
			if err := a.CompleteProbeTask(ctx, taskID, model.ProbeDone, model.ProbeTaskResult{ResourceCheck: &proof}, now); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatal("late completion accepted", err)
			}
			rows, err := a.ListProbeTasksForCase(ctx, id)
			if err != nil || len(rows) != 1 || rows[0].ResourceCheck.Calls != 1 || rows[0].State != model.ProbeCancelled {
				t.Fatal("accepted reservation erased", rows, err)
			}
			if _, err := b.ClaimPendingProbeTasks(ctx, 1); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := r.DB().Model(&qProbeProjectionModel{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("proof became traffic votes", count, err)
			}
		})
	}
}
