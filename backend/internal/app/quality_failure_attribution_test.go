package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type finalReviewEvidence struct{}

func (finalReviewEvidence) SnapshotWindow(time.Time) model.Snapshot { return model.Snapshot{} }
func (finalReviewEvidence) CrossValidate(model.Snapshot) model.Estimate {
	return model.Estimate{}
}

func TestInternalAndLegacyProbeErrorsCannotSentenceAccount(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure model.ProbeFailure
	}{
		{responsebuffer.ErrExhausted.Error(), model.ProbeFailureResource},
		{"completion: " + responsebuffer.ErrLimit.Error(), model.ProbeFailureResource},
		{"physical evidence persistence failed", model.ProbeFailurePersistence},
		{"completion: context deadline exceeded", model.ProbeFailureCompletionBudget},
		{"transport", "transport"}, {"created_timeout", "created_timeout"},
		{"evidence_timeout", "evidence_timeout"}, {"upstream_http", "upstream_http"},
		{"unknown text saying upstream HTTP 503", ""},
		{"typed HTTP server error", model.ProbeFailureHTTPServer},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, now := context.Background(), time.Now().UTC()
			r, err := registry.Open(ctx, registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "review.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			baseline := attemptmeta.Identity{ID: "trigger/1", Provider: "grok_build", Model: "grok-4.6", AccountID: 7, Revision: 1, RuleVersion: "rules-v1", Path: attemptmeta.Path{NodeID: 10, Status: attemptmeta.PathRegistered}, Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "high"}}
			spec := model.NewProbeExperiment(model.Observation{EventID: "trigger/1/admission", Attempt: baseline})
			policy := court.ExperimentPolicy{Version: court.ProtocolVersion, Experiment: spec, AccountPaths: 3, AccountNodes: 2, JurySize: 4, JuryDegraded: 3, TransportPaths: 3, MaxAccountAttempts: 6, MaxJuryAttempts: 8, DeadlineAt: now.Add(time.Minute)}
			raw, err := json.Marshal(map[string]any{"policy": policy})
			if err != nil {
				t.Fatal(err)
			}
			caseID, err := r.OpenInvestigation(ctx, 7, model.EpochKey{NodeID: 10}, now, string(raw))
			if err != nil {
				t.Fatal(err)
			}
			store := registry.NewProbeTaskStore(r)
			identity := func(label string, accountID, nodeID uint64) attemptmeta.Identity {
				v := baseline
				v.ID, v.AccountID, v.Path.NodeID = label, accountID, nodeID
				v.Profile = spec.Profile()
				return v
			}
			for i := uint64(1); i <= 7; i++ {
				task := model.ProbeTask{CaseID: caseID, Direction: model.ProbeAccountDifferential, DefendantAccountID: 7, DefendantNodeID: i, BaselineNodeID: 10, ControlAccountID: 50 + i, ControlNodeID: i}
				if i > 3 {
					task.Direction, task.DefendantNodeID, task.JurorAccountID = model.ProbeExitJury, 10, 50+i
				}
				if _, err := store.CreateProbeTask(ctx, task); err != nil {
					t.Fatal(err)
				}
			}
			tasks, err := store.ClaimPendingProbeTasks(ctx, 7)
			if err != nil || len(tasks) != 7 {
				t.Fatalf("tasks=%d err=%v", len(tasks), err)
			}
			for _, task := range tasks {
				state := model.ProbeDone
				result := model.ProbeTaskResult{Outcome: model.ProbeResultClean, Attempt: identity(fmt.Sprintf("main/%d", task.ID), task.JurorAccountID, 10), PathKey: "incident"}
				if task.Direction == model.ProbeAccountDifferential {
					state = model.ProbeFailed
					result = (model.ProbeMeasurement{Outcome: model.MeasurementError, Reason: test.name, Failure: test.failure, Attempt: identity(fmt.Sprintf("main/%d", task.ID), 7, task.DefendantNodeID), PathKey: fmt.Sprintf("path-%d", task.DefendantNodeID), VerifiedIPChange: true}).TaskResult()
					result.ControlOutcome, result.ControlVerified, result.ControlPathKey = model.ProbeResultClean, true, result.PathKey
					result.ControlAttempt = identity(fmt.Sprintf("control/%d", task.ID), task.ControlAccountID, task.ControlNodeID)
				}
				if err := store.CompleteProbeTask(ctx, task.ID, state, result, now); err != nil {
					t.Fatal(err)
				}
			}
			cfg := court.DefaultConfig()
			cfg.EvaluateEvery = time.Hour
			s := court.New(cfg, r, finalReviewEvidence{}, nil, registry.NewProbeTaskStore(r))
			defer s.Close(context.Background())
			views, err := s.LiveCaseViews(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(views) != 1 || views[0].Assessment == nil {
				t.Fatalf("views=%+v", views)
			}
			report := views[0].Assessment
			wantGuilty := test.failure == model.ProbeFailureHTTPServer
			if (report.Verdict == model.VerdictAccountGuilty) != wantGuilty || (!wantGuilty && report.Account.ConfirmedTransport != 0) {
				t.Fatalf("incorrect evidence qualification: %+v", report)
			}
			if _, err := s.Evaluate(ctx, now); err != nil {
				t.Fatal(err)
			}
			if (r.AccountState(7).State == model.AccountSentenced) != wantGuilty {
				t.Fatalf("incorrect actual scheduling state: %+v", r.AccountState(7))
			}
		})
	}
}
