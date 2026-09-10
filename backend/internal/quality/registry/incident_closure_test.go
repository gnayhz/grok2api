package registry

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestIncidentClosuresAreScopedMonotonicAndTransactional(t *testing.T) {
	r := openTestRegistry(t)
	ctx := context.Background()
	now := time.Now().UTC()
	key := IncidentKey{AccountID: 7, Exit: model.EpochKey{NodeID: 5}}
	id, err := r.OpenInvestigation(ctx, key.AccountID, key.Exit, now, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DB().Exec(`CREATE TRIGGER fail_closure BEFORE INSERT ON q_incident_closure BEGIN SELECT RAISE(ABORT,'injected closure failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictInsufficient, `{}`, now, true); err == nil {
		t.Fatal("closure failure hidden")
	}
	c, _, err := r.GetCase(ctx, id)
	if err != nil || c.Status != model.CaseInvestigating || r.AccountEligible(7) {
		t.Fatalf("partial closure: %+v %v", c, err)
	}
	if err := r.DB().Exec("DROP TRIGGER fail_closure").Error; err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictInsufficient, `{}`, now, true); err != nil {
		t.Fatal(err)
	}
	for _, exit := range []model.EpochKey{{}, {NodeID: 6}, {NodeID: 5, Epoch: 1}} {
		other, err := r.OpenInvestigation(ctx, 7, exit, now, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SettleInvestigation(ctx, other, model.VerdictInsufficient, `{}`, now.Add(time.Minute), true); err != nil {
			t.Fatal(err)
		}
	}
	keys := []IncidentKey{key, {AccountID: 7}, {AccountID: 7, Exit: model.EpochKey{NodeID: 5, Epoch: 1}}, {AccountID: 7, Exit: model.EpochKey{NodeID: 6}}, {AccountID: 8, Exit: key.Exit}}
	got, err := r.LastClosedAtForIncidents(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || !got[key].Equal(now) || !got[keys[1]].Equal(now.Add(time.Minute)) {
		t.Fatalf("incorrect incident scope: %v", got)
	}
	// An older administrative replay must not move the suppression watermark back.
	if err := r.CloseCase(ctx, id, model.CaseDismissed, model.VerdictInsufficient, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err = r.LastClosedAtForIncidents(ctx, []IncidentKey{key})
	if err != nil || !got[key].Equal(now) {
		t.Fatalf("watermark regressed: %v %v", got, err)
	}
	if err := r.ResetQualityState(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = r.LastClosedAtForIncidents(ctx, keys)
	if err != nil || len(got) != 0 {
		t.Fatalf("reset retained closure state: %v %v", got, err)
	}
}

func TestIncidentClosureUpgradeIsAtomicAndPreservesLegacyBaselines(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			r := openTestRegistry(t)
			ctx := context.Background()
			now := time.Now().UTC()
			if driver == "postgres" {
				opts, _ := isolatedQualityPostgres(t)
				var err error
				r, err = Open(ctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
			}
			id, err := r.CreateCase(ctx, now, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range []PartyRecord{
				{CaseID: id, Kind: model.PartyAccount, AccountID: 7, Role: model.RoleDefendant, Disposition: model.DispositionReleased},
				{CaseID: id, Kind: model.PartyExit, NodeID: 5, Epoch: 1, Role: model.RoleCoRemanded, Disposition: model.DispositionReleased},
				{CaseID: id, Kind: model.PartyExit, NodeID: 6, Epoch: 2, Role: model.RoleCoRemanded, Disposition: model.DispositionReleased},
			} {
				if err := r.UpsertParty(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.CloseCase(ctx, id, model.CaseDismissed, model.VerdictInsufficient, now); err != nil {
				t.Fatal(err)
			}
			if err := r.DB().Exec("DELETE FROM q_incident_closure").Error; err != nil {
				t.Fatal(err)
			}
			if err := r.DB().Model(&qStateRevisionModel{}).Where("id=1").Update("incidents_initialized", false).Error; err != nil {
				t.Fatal(err)
			}
			// Make the backfill fail after claiming its initialization marker.
			if err := r.DB().Exec("DROP TABLE q_incident_closure").Error; err != nil {
				t.Fatal(err)
			}
			if err := r.migrateIncidentClosures(ctx); err == nil {
				t.Fatal("failed backfill accepted")
			}
			var state qStateRevisionModel
			if err := r.DB().First(&state, 1).Error; err != nil {
				t.Fatal(err)
			}
			if state.IncidentsInitialized {
				t.Fatal("failed upgrade kept initialization marker")
			}
			if err := r.DB().AutoMigrate(&qIncidentClosureModel{}); err != nil {
				t.Fatal(err)
			}
			if err := r.migrateIncidentClosures(ctx); err != nil {
				t.Fatal(err)
			}
			keys := []IncidentKey{{AccountID: 7, Exit: model.EpochKey{NodeID: 5, Epoch: 1}}, {AccountID: 7, Exit: model.EpochKey{NodeID: 6, Epoch: 2}}}
			got, err := r.LastClosedAtForIncidents(ctx, keys)
			if err != nil || len(got) != 2 {
				t.Fatalf("legacy baselines lost: %v %v", got, err)
			}
			// No source scan on repeat initialization: the source can be unavailable.
			if err := r.DB().Exec("ALTER TABLE q_case_party RENAME TO saved_case_party").Error; err != nil {
				t.Fatal(err)
			}
			if err := r.migrateIncidentClosures(ctx); err != nil {
				t.Fatalf("upgrade rescanned old history: %v", err)
			}
			if err := r.DB().Exec("ALTER TABLE saved_case_party RENAME TO q_case_party").Error; err != nil {
				t.Fatal(err)
			}
		})
	}
}
