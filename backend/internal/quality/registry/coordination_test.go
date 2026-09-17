package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func pairedRegistries(t *testing.T, driver string) (*Registry, *Registry) {
	t.Helper()
	opts := Options{Driver: driver, SQLitePath: filepath.Join(t.TempDir(), "shared.db")}
	if driver == "postgres" {
		opts.PostgresDSN = os.Getenv("GROK_EVOLUTION_POSTGRES_DSN")
		if opts.PostgresDSN == "" {
			t.Skip("set GROK_EVOLUTION_POSTGRES_DSN to an isolated test database")
		}
	}
	first, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	return first, second
}

func TestCrossReplicaStateTransitionsAndAuthority(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			first, second := pairedRegistries(t, driver)
			ctx := context.Background()
			now := time.Now().UTC()
			var wg sync.WaitGroup
			ids := make(chan uint64, 2)
			for _, r := range []*Registry{first, second} {
				wg.Add(1)
				go func(r *Registry) {
					defer wg.Done()
					id, err := r.OpenInvestigation(ctx, 701, model.EpochKey{NodeID: 901}, now, `{}`)
					if err != nil {
						t.Error(err)
					}
					ids <- id
				}(r)
			}
			wg.Wait()
			id, duplicate := <-ids, <-ids
			if id == 0 || duplicate != id {
				t.Fatalf("duplicate incident: %d %d", id, duplicate)
			}
			// A second instance may still have an old candidate snapshot, but its
			// lease check must immediately see the authoritative restriction.
			if allowed, err := second.ExitAllowed(ctx, 901); err != nil || allowed {
				t.Fatalf("peer restriction missed: %v %v", allowed, err)
			}
			if err := first.SettleInvestigation(ctx, id, model.VerdictAccountGuilty, `{}`, now, true); err != nil {
				t.Fatal(err)
			}
			// Another incident must not overwrite a peer's conviction with remand.
			otherID, err := second.OpenInvestigation(ctx, 701, model.EpochKey{NodeID: 902}, now, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			if second.AccountState(701).State != model.AccountSentenced {
				t.Fatal("stale snapshot erased conviction")
			}
			if err := second.SettleInvestigation(ctx, otherID, model.VerdictInsufficient, `{}`, now, true); err != nil {
				t.Fatal(err)
			}
			if err := first.RefreshState(ctx); err != nil {
				t.Fatal(err)
			}
			if first.AccountState(701).State != model.AccountSentenced {
				t.Fatal("independent release erased conviction")
			}
			if err := first.RecordExitIdentity(ctx, 903, model.ExitIdentityFromAggregate("192.0.2.1")); err != nil {
				t.Fatal(err)
			}
			if _, _, err := first.AdvanceEpoch(ctx, 903, model.ExitIdentityFromAggregate("192.0.2.2")); err != nil {
				t.Fatal(err)
			}
			if epoch, _, err := second.AdvanceEpoch(ctx, 903, model.ExitIdentityFromAggregate("192.0.2.3")); err != nil || epoch != 2 {
				t.Fatalf("stale epoch allocation: %d %v", epoch, err)
			}
			if epoch, released, err := first.AdvanceEpoch(ctx, 903, model.ExitIdentityFromAggregate("192.0.2.3")); err != nil || epoch != 2 || len(released) != 0 {
				t.Fatalf("peer IP observation advanced twice: %d %+v %v", epoch, released, err)
			}
		})
	}
}

func TestCoordinatorFencesOldWriterAfterTakeover(t *testing.T) {
	first, second := pairedRegistries(t, "sqlite")
	ctx := context.Background()
	oldCtx, releaseOld, err := first.Coordinate(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOld()
	if err := first.DB().Model(&qCoordinationModel{}).Where("name = ?", "test").Update("until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	newCtx, releaseNew, err := second.Coordinate(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNew()
	if err := first.TransitionAccount(oldCtx, model.AccountTransitionRequest{AccountID: 42, To: model.AccountRemanded, CaseID: 1}); err == nil {
		t.Fatal("old coordinator wrote after takeover")
	}
	if err := second.TransitionAccount(newCtx, model.AccountTransitionRequest{AccountID: 42, To: model.AccountRemanded, CaseID: 1}); err != nil {
		t.Fatal(err)
	}
	releaseOld()
	if err := second.TransitionAccount(newCtx, model.AccountTransitionRequest{AccountID: 43, To: model.AccountRemanded, CaseID: 1}); err != nil {
		t.Fatalf("old release unlocked peer: %v", err)
	}
}

func TestProbeTaskInheritsFrozenCaseExperiment(t *testing.T) {
	r := newReconcileRegistry(t)
	ctx := context.Background()
	spec := model.ProbeExperiment{Version: model.ProbeExperimentVersion, TriggerEventID: "trigger", Sample: "ordering"}
	raw, err := json.Marshal(map[string]any{"policy": map[string]any{"experiment": spec}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.OpenInvestigation(ctx, 4, model.EpochKey{NodeID: 8}, time.Now().UTC(), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	store := NewProbeTaskStore(r)
	if _, err := store.CreateProbeTask(ctx, model.ProbeTask{CaseID: id, Direction: model.ProbeExitJury, Experiment: model.ProbeExperiment{Sample: "changed"}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ClaimPendingProbeTasks(ctx, 1)
	if err != nil || len(tasks) != 1 || tasks[0].Experiment != spec {
		t.Fatalf("spec changed: %+v %v", tasks, err)
	}
}
