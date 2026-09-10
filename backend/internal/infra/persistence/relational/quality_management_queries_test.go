package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/guard"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type managementNodeProfiles struct{ db *gorm.DB }

func (s managementNodeProfiles) ListProfiles(ctx context.Context) ([]proxy.NodeProfile, error) {
	var rows []egressNodeModel
	if err := s.db.WithContext(ctx).Select("id, rotation_enabled").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]proxy.NodeProfile, 0, len(rows))
	for _, r := range rows {
		values = append(values, proxy.NodeProfile{ID: r.ID, RotationWebhook: r.RotationEnabled})
	}
	return values, nil
}

func qualityManagementPair(t *testing.T, dialect string) (*registry.Registry, *registry.Registry, *Database) {
	t.Helper()
	db, _ := settingsDatabasePair(t, dialect)
	opts := registry.Options{Driver: dialect, AccountLinks: NewAccountRepository(db)}
	if dialect == "postgres" {
		opts.PostgresDSN = db.db.Dialector.(*postgres.Dialector).DSN
	} else {
		var files []struct{ Name, File string }
		if err := db.db.Raw("PRAGMA database_list").Scan(&files).Error; err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if file.Name == "main" {
				opts.SQLitePath = file.File
			}
		}
		if opts.SQLitePath == "" {
			t.Fatal("missing database file")
		}
	}
	open := func() *registry.Registry {
		reg, err := registry.Open(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reg.Close() })
		return reg
	}
	return open(), open(), db
}

func managementQueriesOn(t *testing.T, reg *registry.Registry, db *Database) (*management.Queries, *evidence.Store) {
	t.Helper()
	observations, err := evidence.New(context.Background(), reg.DB(), evidence.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := court.DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := court.New(cfg, reg, observations, nil)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	return management.NewQueries(management.QueryDependencies{Registry: reg, Evidence: observations, Court: service, Probes: registry.NewProbeTaskStore(reg), Guard: guard.New(guard.DefaultConfig(), nil), Nodes: managementNodeProfiles{db.db}}), observations
}

func TestQualityManagementQueriesRefreshAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b, db := qualityManagementPair(t, dialect)
			ctx := context.Background()
			queries, observations := managementQueriesOn(t, b, db)
			if err := db.db.Create(&egressNodeModel{ID: 7, Name: "quality-node", Enabled: true, RotationEnabled: true}).Error; err != nil {
				t.Fatal(err)
			}
			first, _, err := a.AdvanceEpoch(ctx, 7, "198.51.100.1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.OpenInvestigation(ctx, 42, qualitymodel.EpochKey{NodeID: 7, Epoch: first}, time.Now().UTC(), "{}"); err != nil {
				t.Fatal(err)
			}
			if err := observations.Record(ctx, qualitymodel.Observation{AccountID: 42, Exit: qualitymodel.EpochKey{NodeID: 7, Epoch: first}, At: time.Now().UTC(), Source: qualitymodel.SourceTraffic, Outcome: qualitymodel.OutcomeDegraded}); err != nil {
				t.Fatal(err)
			}
			// No notification or explicit refresh of B. Archive SQL must pair the state
			// with the selected epoch even though B was constructed before the incident.
			nodes, err := queries.Egress(ctx)
			if err != nil || len(nodes) != 1 || nodes[0].State != qualitymodel.ExitRemanded || !nodes[0].Webhook {
				t.Fatalf("archive=%+v err=%v", nodes, err)
			}
			matrix, err := queries.Matrix(ctx)
			if err != nil || len(matrix.Accounts) != 1 || matrix.Accounts[0].State != qualitymodel.AccountRemanded || len(matrix.Exits) != 1 || matrix.Exits[0].State != qualitymodel.ExitRemanded {
				t.Fatalf("matrix=%+v err=%v", matrix, err)
			}
			// The caller owns these maps; modifying the returned projection must not
			// corrupt the registry's scheduling cache or any later query.
			state, err := b.ManagementState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			delete(state.Accounts, 42)
			delete(state.Exits, qualitymodel.EpochKey{NodeID: 7, Epoch: first})
			if b.AccountEligible(42) || b.ExitEligible(7) {
				t.Fatal("management mutation changed eligibility")
			}
			second, _, err := a.AdvanceEpoch(ctx, 7, "198.51.100.2")
			if err != nil {
				t.Fatal(err)
			}
			if err := a.TransitionExit(ctx, registry.ExitTransitionRequest{NodeID: 7, Epoch: second, To: qualitymodel.ExitRemanded, CaseID: 1}); err != nil {
				t.Fatal(err)
			}
			if err := observations.Record(ctx, qualitymodel.Observation{AccountID: 42, Exit: qualitymodel.EpochKey{NodeID: 7, Epoch: second}, At: time.Now().UTC(), Source: qualitymodel.SourceTraffic, Outcome: qualitymodel.OutcomeDelivered}); err != nil {
				t.Fatal(err)
			}
			matrix, err = queries.Matrix(ctx)
			if err != nil || len(matrix.Exits) != 2 {
				t.Fatalf("matrix=%+v err=%v", matrix, err)
			}
			for _, exit := range matrix.Exits {
				want := qualitymodel.ExitAvailable
				if exit.Key.Epoch == second {
					want = qualitymodel.ExitRemanded
				}
				if exit.State != want {
					t.Fatalf("epoch state=%+v want=%s", exit, want)
				}
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := queries.Matrix(cancelled); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled query=%v", err)
			}
			if err := a.DB().Exec("DROP TABLE q_state_revision").Error; err != nil {
				t.Fatal(err)
			}
			if _, err := queries.Matrix(ctx); err == nil {
				t.Fatal("failed refresh exposed stale matrix")
			}
		})
	}
}

func TestQualityManagementSnapshotKeepsAtomicIncidentAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b, _ := qualityManagementPair(t, dialect)
			ctx := context.Background()
			key := qualitymodel.EpochKey{NodeID: 7}
			done := make(chan error, 1)
			go func() {
				for i := 0; i < 20; i++ {
					id, err := a.OpenInvestigation(ctx, 42, key, time.Now().UTC(), "{}")
					if err != nil {
						done <- err
						return
					}
					if err := a.SettleInvestigation(ctx, id, qualitymodel.VerdictInsufficient, "{}", time.Now().UTC(), true); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			// Join the writer even on an assertion failure before releasing either pool.
			var result error
			for i := 0; i < 40; i++ {
				state, err := b.ManagementState(ctx)
				if err != nil {
					result = err
					break
				}
				accountHeld := !state.Accounts[42].State.Schedulable()
				// Sparse defaults represent active/available.
				if state.Accounts[42].State == "" {
					accountHeld = false
				}
				exitHeld := !state.Exits[key].State.Schedulable()
				if state.Exits[key].State == "" {
					exitHeld = false
				}
				if accountHeld != exitHeld {
					result = errors.New("mixed account/exit incident revision")
					break
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if result != nil {
				t.Fatal(result)
			}
		})
	}
}
