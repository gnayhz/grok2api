package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestQualityNodeFactsDriveCourtAndEpochs(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			dsn := os.Getenv("TEST_POSTGRES_DSN")
			if driver == "postgres" && dsn == "" {
				t.Skip("requires isolated TEST_POSTGRES_DSN")
			}
			var path string
			a := newLifecycleApplication(t, func(cfg *config.Config) {
				path = cfg.Database.SQLite.Path
				if driver == "postgres" {
					cfg.Database.Driver = driver
					cfg.Database.Postgres.DSN = dsn
				}
			})
			if err := a.qualityCourt.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := a.qualityEnforcement.Close(ctx); err != nil {
				t.Fatal(err)
			}
			// A second main-store connection mutates facts observed by the
			// application's egress service and independent quality SQL pool.
			var db *relational.Database
			var err error
			if driver == "sqlite" {
				db, err = relational.OpenSQLite(ctx, path)
			} else {
				db, err = relational.OpenPostgres(ctx, dsn, 4, 2)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			nodes := relational.NewEgressRepository(db)
			var defendant uint64
			for i := range 7 {
				c, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: fmt.Sprintf("node-facts-%d", i), SourceKey: fmt.Sprintf("node-facts-%d", i), Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					defendant = c.ID
				}
				if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, c.ID, []string{"grok-4.6"}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
				t.Fatal(err)
			}
			var baseline egressdomain.Node
			allowed := map[uint64]bool{}
			named := map[string]egressdomain.Node{}
			for _, name := range []string{"baseline_pool", "healthy_1", "healthy_2", "healthy_3", "disabled", "cooling", "unconfigured", "deleted"} {
				n, err := nodes.CreateEgressNode(ctx, egressdomain.Node{Name: name, Enabled: true, Health: 1, EncryptedProxyURL: "fixture-proxy", ProxyPool: name == "baseline_pool"})
				if err != nil {
					t.Fatal(err)
				}
				named[name] = n
				if name == "baseline_pool" {
					baseline = n
					continue
				}
				if err := a.qualityEvidence.Record(ctx, model.Observation{At: time.Now(), AccountID: defendant, Exit: model.EpochKey{NodeID: n.ID}, Source: model.SourceTraffic, Outcome: model.OutcomeDelivered}); err != nil {
					t.Fatal(err)
				}
				switch name {
				case "healthy_1", "healthy_2", "healthy_3":
					allowed[n.ID] = true
				case "disabled":
					n.Enabled = false
				case "cooling":
					until := time.Now().Add(time.Hour)
					n.CooldownUntil = &until
				case "unconfigured":
					n.EncryptedProxyURL = ""
				case "deleted":
					if err := nodes.DeleteEgressNode(ctx, n.ID); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if _, err := nodes.UpdateEgressNodeConfiguration(ctx, n, func(egressdomain.Node) error {
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if name == "cooling" {
					current, err := nodes.GetEgressNode(ctx, n.ID)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := nodes.ApplyEgressHealthObservation(ctx, egressdomain.HealthObservation{NodeID: current.ID, EncryptedProxyURL: current.EncryptedProxyURL, BindingRevision: current.BindingRevision, Kind: egressdomain.HealthTransportFailure, Failures: 2, CooldownUntil: n.CooldownUntil, ObservedAt: time.Now().UTC()}); err != nil {
						t.Fatal(err)
					}
				}
			}
			estimate := a.qualityEvidence.CrossValidate(a.qualityEvidence.SnapshotWindow(time.Now()))
			for _, name := range []string{"disabled", "cooling", "unconfigured", "deleted"} {
				if _, ok := estimate.Exits[model.EpochKey{NodeID: named[name].ID}]; !ok {
					t.Fatalf("missing historical observation for %s", name)
				}
			}
			obs := model.Observation{At: time.Now(), AccountID: defendant, Exit: model.EpochKey{NodeID: baseline.ID}, Source: model.SourceTraffic, Outcome: model.OutcomeDegraded, EventID: "node-facts-incident", Attempt: attemptmeta.Identity{ID: "node-facts-trigger", Provider: "grok_build", Model: "grok-4.6", RuleVersion: "reasoning-v1", Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "high"}}}
			// Initial candidate failure creates a recoverable case, no tasks.
			if err := a.quality.DB().Exec("ALTER TABLE egress_nodes RENAME TO e12_unavailable_nodes").Error; err != nil {
				t.Fatal(err)
			}
			missing := true
			restore := func() {
				if missing {
					if err := a.quality.DB().Exec("ALTER TABLE e12_unavailable_nodes RENAME TO egress_nodes").Error; err != nil {
						t.Fatal(err)
					}
					missing = false
				}
			}
			t.Cleanup(restore)
			if err := a.qualityCourt.ReportDegradedObservation(ctx, obs); err == nil {
				t.Fatal("unavailable nodes accepted as empty candidates")
			}
			cases, err := a.quality.ListOpenCases(ctx)
			if err != nil || len(cases) != 1 {
				t.Fatalf("recoverable case missing: %d %v", len(cases), err)
			}
			id := cases[0].ID
			store := registry.NewProbeTaskStore(a.quality)
			if tasks, err := store.ListProbeTasksForCase(ctx, id); err != nil || len(tasks) != 0 {
				t.Fatalf("unreadable candidates emitted tasks: %d %v", len(tasks), err)
			}
			if _, err := a.qualityCourt.Evaluate(ctx, time.Now()); err == nil {
				t.Fatal("replacement treated read failure as exhaustion")
			}
			if _, _, _, err := (baseExitIPSource{egress: a.egressOps}).CurrentExitIP(ctx, baseline.ID); err == nil {
				t.Fatal("unreadable IP observation became absence")
			}
			if _, err := a.qualityEnforcement.PollEpochs(ctx); err == nil {
				t.Fatal("epoch source failed silently")
			}
			restore()
			if stats, err := a.qualityCourt.Evaluate(ctx, time.Now()); err != nil || stats.Retried == 0 {
				t.Fatalf("read recovery did not resume plan: %+v %v", stats, err)
			}
			tasks, err := store.ClaimPendingProbeTasks(ctx, 32)
			if err != nil || len(tasks) < 6 {
				t.Fatalf("missing real investigation tasks: %d %v", len(tasks), err)
			}
			for _, task := range tasks {
				if task.Direction == model.ProbeAccountDifferential && !allowed[task.DefendantNodeID] {
					t.Fatalf("unusable historical/fleet candidate: %+v", task)
				}
				if !allowed[task.ControlNodeID] {
					t.Fatalf("unusable control path: %+v", task)
				}
				identity := func(label string, accountID, nodeID, epoch uint64) attemptmeta.Identity {
					value := obs.Attempt
					value.ID = fmt.Sprintf("%s-%d", label, task.ID)
					value.AccountID = accountID
					value.Path = attemptmeta.Path{NodeID: nodeID, Epoch: epoch, Status: attemptmeta.PathRegistered}
					value.Profile.Experiment, value.Profile.Sample = task.Experiment.Version, task.Experiment.Sample
					return value
				}
				accountID := task.DefendantAccountID
				outcome := model.ProbeResultClean
				if task.Direction == model.ProbeExitJury {
					accountID, outcome = task.JurorAccountID, model.ProbeResultDegraded
				}
				result := model.ProbeTaskResult{Outcome: outcome, VerifiedIPChange: true, PathKey: fmt.Sprintf("node-%d", task.DefendantNodeID), Attempt: identity("main", accountID, task.DefendantNodeID, task.DefendantEpoch), ControlOutcome: model.ProbeResultClean, ControlVerified: true, ControlPathKey: fmt.Sprintf("node-%d", task.ControlNodeID), ControlAttempt: identity("control", task.ControlAccountID, task.ControlNodeID, task.ControlEpoch)}
				if err := store.CompleteProbeTask(ctx, task.ID, model.ProbeDone, result, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			// Existing measurements cannot turn an unreadable pool into fixed.
			if err := a.quality.DB().Exec("ALTER TABLE egress_nodes RENAME TO e12_unavailable_nodes").Error; err != nil {
				t.Fatal(err)
			}
			missing = true
			if _, err := a.qualityCourt.Evaluate(ctx, time.Now()); err == nil {
				t.Fatal("unreadable node type became a ban decision")
			}
			record, _, err := a.quality.GetCase(ctx, id)
			if err != nil || record.Status.Closed() || a.quality.ExitStateOfCurrentEpoch(baseline.ID).State != model.ExitRemanded {
				t.Fatalf("type read failure changed disposition: %+v %v", record, err)
			}
			restore()
			if _, err := a.qualityCourt.Evaluate(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			record, _, err = a.quality.GetCase(ctx, id)
			if err != nil || record.Verdict != model.VerdictExitGuilty || a.quality.ExitStateOfCurrentEpoch(baseline.ID).State != model.ExitRemanded || !a.quality.AccountEligible(defendant) {
				t.Fatalf("pool verdict lost: %+v %v", record, err)
			}
			// The operational projection preserves not-found vs cancelled reads.
			source := baseNodeSource{egress: a.egressOps}
			if _, found, err := source.Profile(ctx, named["deleted"].ID); err != nil || found {
				t.Fatalf("deleted fact=%v %v", found, err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := source.ListProfiles(cancelled); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled list=%v", err)
			}
			if _, _, err := source.Profile(cancelled, baseline.ID); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled profile=%v", err)
			}
			ipSource := baseExitIPSource{egress: a.egressOps}
			if _, _, _, err := ipSource.CurrentExitIP(cancelled, baseline.ID); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled IP=%v", err)
			}
			// A known transport observation changes the epoch once and releases
			// only the held old identity. Repeating it cannot release a new ban.
			for revision, ip := range []string{"192.0.2.1", "192.0.2.2"} {
				v, err := nodes.BeginEgressNodeProbe(ctx, baseline.ID, baseline.EncryptedProxyURL)
				if err != nil || v != uint64(revision+1) {
					t.Fatalf("probe revision=%d %v", v, err)
				}
				if err := nodes.UpdateEgressNodeProbe(ctx, baseline.ID, baseline.EncryptedProxyURL, egressdomain.ProbeResult{Revision: v, Status: egressdomain.ProbeStatusHealthy, TestedAt: time.Now(), ExitIP: ip}); err != nil {
					t.Fatal(err)
				}
				if _, err := a.qualityEnforcement.PollEpochs(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if a.quality.CurrentEpoch(baseline.ID) != 1 || a.quality.ExitStateOfCurrentEpoch(baseline.ID).State != model.ExitAvailable {
				t.Fatal("current IP did not advance and release old pool hold")
			}
			if changes, err := a.qualityEnforcement.PollEpochs(ctx); err != nil || len(changes) != 0 {
				t.Fatalf("duplicate observation changed epoch: %+v %v", changes, err)
			}
		})
	}
}
