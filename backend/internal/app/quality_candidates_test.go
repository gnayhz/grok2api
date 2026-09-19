package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

type captureCandidatePlan struct{ plans []court.DispatchSpec }

func (d *captureCandidatePlan) DispatchForCase(_ context.Context, spec court.DispatchSpec) (int, error) {
	d.plans = append(d.plans, spec)
	return 1, nil
}

func TestCourtCandidatesUseFrozenModelEligibility(t *testing.T) {
	ctx := context.Background()
	a := newLifecycleApplication(t)
	if err := a.qualityCourt.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for _, name := range []string{"defendant", "eligible", "other-model"} {
		c, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: name, SourceKey: name, Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "unused"})
		if err != nil {
			t.Fatal(err)
		}
		models := []string{"grok-4.6"}
		if name == "other-model" {
			models = []string{"grok-3"}
		}
		if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, c.ID, models, time.Now()); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-3", "grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	dispatch := &captureCandidatePlan{}
	cfg := court.DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	a.qualityCourt = court.New(cfg, a.quality, qualityEvidenceSource{store: a.qualityEvidence}, dispatch, registry.NewProbeTaskStore(a.quality))
	a.qualityCourt.SetNodes(baseNodeSource{egress: a.egressOps})
	a.qualityCourt.SetProbeAccounts(a.gateway)
	obs := model.Observation{At: time.Now(), AccountID: ids[0], Exit: model.EpochKey{NodeID: 99}, Source: model.SourceTraffic, Outcome: model.OutcomeDegraded, EventID: "frozen-model", Attempt: attemptmeta.Identity{ID: "frozen-model", Provider: "grok_build", Model: "grok-4.6", RuleVersion: "reasoning-v1", Profile: attemptmeta.Profile{Known: true, Protocol: "responses"}}}
	if err := a.qualityCourt.ReportDegradedObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	if len(dispatch.plans) != 1 {
		t.Fatalf("plans=%+v", dispatch.plans)
	}
	records, err := a.quality.ListOpenCases(ctx)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	var envelope struct {
		Policy court.ExperimentPolicy `json:"policy"`
	}
	if err := json.Unmarshal([]byte(records[0].EvidenceJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	jurors := envelope.Policy.Experiment.ResourceCheck.Accounts
	if len(jurors) != 1 || jurors[0] != ids[1] {
		t.Fatalf("frozen grok-4.6 jury=%v; want only capable account %d (unsupported account %d)", jurors, ids[1], ids[2])
	}
	// A real account fact read failure is observable, not an empty candidate pool.
	if err := a.quality.DB().Exec("ALTER TABLE account_billing_snapshots RENAME TO e11_unavailable_billing").Error; err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if err := a.quality.DB().Exec("ALTER TABLE e11_unavailable_billing RENAME TO account_billing_snapshots").Error; err != nil {
			t.Error(err)
		}
	}
	failedObs := obs
	failedObs.Exit.NodeID = 100
	failedObs.EventID = "read-failed"
	readErr := a.qualityCourt.ReportDegradedObservation(ctx, failedObs)
	restore()
	if readErr == nil {
		t.Fatal("real account storage failure swallowed")
	}
	if len(dispatch.plans) != 1 {
		t.Fatal("read failure dispatched incomplete candidates")
	}
	open, err := a.quality.ListOpenCases(ctx)
	if err != nil || len(open) != 2 {
		t.Fatalf("read failure discarded case: %+v %v", open, err)
	}
	if _, err := a.qualityCourt.Evaluate(ctx, time.Now()); err != nil {
		t.Fatalf("restored facts did not recover: %v", err)
	}
	if len(dispatch.plans) < 3 {
		t.Fatal("restored facts did not dispatch pending cases")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.gateway.QualityProbeAccounts(cancelled, model.NewProbeExperiment(obs)); !errors.Is(err, context.Canceled) {
		t.Fatalf("real candidate cancellation lost: %v", err)
	}

}

// Production Court -> Gateway/Selector -> investigator -> SQL task storage.
// No worker is started, so candidate planning cannot generate upstream work.
func TestApplicationPersistsOnlyEligibleProbeCandidates(t *testing.T) {
	ctx := context.Background()
	a := newLifecycleApplication(t)
	var defendant uint64
	allowed := map[uint64]bool{}
	for i := range 8 {
		name := fmt.Sprintf("candidate-%d", i)
		c, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: name, SourceKey: name, Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			defendant = c.ID
		}
		models := []string{"grok-4.6"}
		if i == 6 {
			models = []string{"grok-3"}
		}
		if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, c.ID, models, time.Now()); err != nil {
			t.Fatal(err)
		}
		if i == 7 {
			if _, err := a.accountRepo.ApplyModelRestriction(ctx, c.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: account.ModelAccessDenied, UpstreamModel: "grok-4.6", RetryAfter: time.Hour}); err != nil {
				t.Fatal(err)
			}
		} else if i > 0 && i < 6 {
			allowed[c.ID] = true
		}
	}
	if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-3", "grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	nodes := relational.NewEgressRepository(a.database)
	var baseline model.EpochKey
	for i := range 3 {
		node, err := nodes.CreateEgressNode(ctx, egressdomain.Node{Name: fmt.Sprintf("candidate-path-%d", i), Enabled: true, Health: 1, EncryptedProxyURL: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		epoch, _, err := a.quality.AdvanceEpoch(ctx, node.ID, model.ExitIdentityFromAggregate(fmt.Sprintf("198.51.100.%d", i+1)))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			baseline = model.EpochKey{NodeID: node.ID, Epoch: epoch}
		}
	}
	obs := model.Observation{At: time.Now(), AccountID: defendant, Exit: baseline, Source: model.SourceTraffic, Outcome: model.OutcomeDegraded, EventID: "candidate-incident", Attempt: attemptmeta.Identity{ID: "candidate-attempt", Provider: "grok_build", Model: "grok-4.6", RuleVersion: "reasoning-v1", Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "high"}}}
	if err := a.qualityCourt.ReportDegradedObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	cases, err := a.quality.ListOpenCases(ctx)
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases=%+v err=%v", cases, err)
	}
	tasks, err := registry.NewProbeTaskStore(a.quality).ListProbeTasksForCase(ctx, cases[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Direction != model.ProbeCaseProof {
		t.Fatalf("expected one shared proof task: tasks=%d", len(tasks))
	}
	for _, task := range tasks {
		if task.Experiment.Baseline != obs.Attempt || task.Experiment.Version != model.ResourceCheckVersion {
			t.Fatal("persisted experiment changed")
		}
		plan := task.Experiment.ResourceCheck
		if len(plan.Nodes) != 2 || len(plan.Accounts) != len(allowed) || plan.MaxCalls != 6 {
			t.Fatalf("unexpected plan %+v", plan)
		}
		for _, id := range plan.Accounts {
			if !allowed[id] {
				t.Fatal("ineligible account in plan", id)
			}
		}

		if task.State != model.ProbePending {
			t.Fatalf("planning executed a measurement: %+v", task)
		}
	}
}
