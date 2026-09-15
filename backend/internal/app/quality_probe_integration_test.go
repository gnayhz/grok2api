package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/investigator"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

// The local server represents the upstream generation. Network leases and path
// labeling are real; only public exit-address discovery uses synthetic addresses.
type probeIntegrationAddresses map[uint64]egressdomain.ExitAddresses

func (a probeIntegrationAddresses) NodeExitAddrs(_ context.Context, id uint64) (egressdomain.ExitAddresses, error) {
	return a[id], nil
}

func TestApplicationProbeExecutionThroughTaskAndCourt(t *testing.T) {
	for _, scenario := range []string{"clean", "degraded_with_control", "server_error_with_control", "incomplete", "stale_before", "stale_inflight", "control_stale_inflight", "cancel_primary", "cancel_control", "legacy_baseline", "policy_changed", "jury_clean", "jury_degraded_with_control", "jury_server_error_with_control", "jury_incomplete", "jury_control_stale_inflight", "jury_cancel_primary", "jury_cancel_control", "jury_policy_changed", "epoch_read_failed_before", "epoch_read_failed_after_primary", "epoch_read_failed_after_control", "identity_changed", "jury_identity_changed"} {
		t.Run(scenario, func(t *testing.T) {
			jury := strings.HasPrefix(scenario, "jury_")
			scenario := strings.TrimPrefix(scenario, "jury_")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			var onRequest func(int)
			var spec model.ProbeExperiment
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/responses" {
					http.Error(w, "fixture endpoint", 404)
					return
				}
				n := int(calls.Add(1))
				var body struct {
					Input     string `json:"input"`
					Model     string `json:"model"`
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body.Input != spec.Prompt() || body.Model != "grok-4.6" || body.Reasoning.Effort != spec.Profile().ReasoningEffort {
					t.Errorf("probe experiment changed: %+v", body)
				}
				if onRequest != nil {
					onRequest(n)
				}
				if scenario == "server_error_with_control" && n == 1 {
					http.Error(w, "synthetic upstream failure", 503)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"probe-response\",\"status\":\"in_progress\"}}\n\n")
				clean := scenario == "clean" || scenario == "incomplete" || scenario == "legacy_baseline" || scenario == "identity_changed" || n > 1
				if clean {
					io.WriteString(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"calculate the totals\"}\n\n")
				}
				io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"The final answer is 64.\"}\n\n")
				status := "completed"
				if scenario == "incomplete" {
					status = "incomplete"
				}
				fmt.Fprintf(w, "data: {\"type\":\"response.%s\",\"response\":{\"id\":\"probe-response\",\"status\":\"%s\",\"usage\":{\"input_tokens\":20,\"output_tokens\":64}}}\n\n", status, status)
			}))
			defer upstream.Close()
			var sqlitePath string
			a := newLifecycleApplication(t, func(cfg *config.Config) {
				cfg.Provider.Build.BaseURL = upstream.URL + "/v1"
				cfg.Provider.Build.FallbackBaseURL = "disabled"
				cfg.Provider.Web.BaseURL = upstream.URL
				cfg.Provider.Console.BaseURL = upstream.URL
				sqlitePath = cfg.Database.SQLite.Path
			})
			if _, ok := a.qualityProbeExec.(*investigator.ProbeExecutor); !ok {
				t.Fatalf("production executor type=%T", a.qualityProbeExec)
			}
			if err := a.qualityCourt.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			cfg := court.DefaultConfig()
			cfg.EvaluateEvery = time.Hour
			cfg.Logger = a.logger
			a.qualityCourt = court.New(cfg, a.quality, qualityEvidenceSource{store: a.qualityEvidence}, nil)
			a.qualityCourt.SetNodes(baseNodeSource{egress: a.egressOps})
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			token, err := cipher.Encrypt("synthetic-probe-token")
			if err != nil {
				t.Fatal(err)
			}
			var ids []uint64
			maxConcurrent := 1
			if scenario == "identity_changed" {
				maxConcurrent = 4
			}
			for i := range 2 {
				credential, _, err := a.accountRepo.UpsertByIdentity(context.Background(), account.Credential{Provider: account.ProviderBuild, Name: fmt.Sprintf("probe-%d", i), SourceKey: fmt.Sprintf("probe-%d", i), EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: maxConcurrent})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, credential.ID)
				if err := testsupport.Capabilities(context.Background(), a.modelRepo, a.accountRepo, credential.ID, []string{"grok-4.6"}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := testsupport.Discover(context.Background(), a.modelRepo, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
				t.Fatal(err)
			}
			networkRepo := relational.NewEgressRepository(a.database)
			var nodes []uint64
			addresses := probeIntegrationAddresses{}
			for i := range 2 {
				target, err := url.Parse(upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
				defer proxy.Close()
				encryptedProxy, err := cipher.Encrypt(proxy.URL)
				if err != nil {
					t.Fatal(err)
				}
				node, err := networkRepo.CreateEgressNode(context.Background(), egressdomain.Node{Name: fmt.Sprintf("probe-path-%d", i), Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy})
				if err != nil {
					t.Fatal(err)
				}
				nodes = append(nodes, node.ID)
				address := fmt.Sprintf("198.51.100.%d", i+1)
				addresses[node.ID] = egressdomain.ExitAddresses{IPv4: address}
				if _, _, err := a.quality.AdvanceEpoch(context.Background(), node.ID, address); err != nil {
					t.Fatal(err)
				}
			}
			a.gateway.SetNodeExitIPResolver(addresses)
			peer, err := registry.Open(context.Background(), registry.Options{Driver: "sqlite", SQLitePath: sqlitePath, AccountLinks: a.accountRepo.(registry.AccountLinks)})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			snapshot := (qualityGuardSnapshotSource{service: a.qualityGuard, registry: a.quality}).GuardSnapshot()
			baseline := attemptmeta.Identity{ID: "opening/1", AccountID: ids[0], Provider: "grok_build", Model: "grok-4.6", Revision: snapshot.Runtime.Revision, RuleVersion: snapshot.Runtime.RuleVersion, Path: attemptmeta.Path{NodeID: nodes[0], Epoch: 1, Status: attemptmeta.PathRegistered}, Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "high"}}
			spec = model.NewProbeExperiment(model.Observation{EventID: "opening/1/admission", Attempt: baseline})
			if scenario == "policy_changed" {
				spec.Baseline.Revision++
			}
			now := time.Now().UTC()
			policy := court.ExperimentPolicy{Version: court.ProtocolVersion, Experiment: spec, AccountPaths: 3, AccountNodes: 2, JurySize: 4, JuryDegraded: 3, TransportPaths: 3, MaxAccountAttempts: 6, MaxJuryAttempts: 8, DeadlineAt: now.Add(time.Hour)}
			envelope, err := json.Marshal(map[string]any{"policy": policy, "exit": map[string]any{"node": nodes[0], "epoch": 1}})
			if err != nil {
				t.Fatal(err)
			}
			caseID, err := a.quality.OpenInvestigation(context.Background(), ids[0], model.EpochKey{NodeID: nodes[0], Epoch: 1}, now, string(envelope))
			if err != nil {
				t.Fatal(err)
			}
			task := model.ProbeTask{CaseID: caseID, Direction: model.ProbeAccountDifferential, DefendantAccountID: ids[0], DefendantNodeID: nodes[1], DefendantEpoch: 1, BaselineNodeID: nodes[0], BaselineEpoch: 1, ControlAccountID: ids[1], ControlNodeID: nodes[1], ControlEpoch: 1, Experiment: spec}
			if jury {
				task.Direction = model.ProbeExitJury
				task.JurorAccountID = ids[1]
				task.DefendantNodeID = nodes[0]
			}
			if scenario == "incomplete" || scenario == "policy_changed" {
				task.ControlAccountID = 0
			}
			if scenario == "legacy_baseline" {
				task.BaselineNodeID = 0
				task.BaselineEpoch = 0
			}
			tasks := registry.NewProbeTaskStore(a.quality)
			if _, err := tasks.CreateProbeTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			advance := func() {
				if _, _, err := peer.AdvanceEpoch(context.Background(), nodes[1], "198.51.100.33"); err != nil {
					t.Error(err)
				}
			}
			if scenario == "stale_before" {
				advance()
			}
			hideEpochs := func() {
				if err := peer.DB().Migrator().RenameTable("q_node_epoch", "e08_hidden_epoch"); err != nil {
					t.Error(err)
				}
			}
			if scenario == "epoch_read_failed_before" {
				hideEpochs()
			}
			onRequest = func(n int) {
				if scenario == "identity_changed" && n == 1 {
					subject := task.DefendantAccountID
					if jury {
						subject = task.JurorAccountID
					}
					web, _, err := a.accountRepo.UpsertByIdentity(context.Background(), account.Credential{Provider: account.ProviderWeb, Name: "identity-peer", SourceKey: "identity-peer", EncryptedAccessToken: token, Enabled: true, AuthStatus: account.AuthStatusActive})
					if err != nil {
						t.Error(err)
						return
					}
					build, err := a.accountRepo.Get(context.Background(), subject)
					if err != nil {
						t.Error(err)
						return
					}
					if err := a.accountRepo.LinkWebToBuild(context.Background(), web.CredentialRef(), build.CredentialRef()); err != nil {
						t.Error(err)
						return
					}
					if err := peer.RefreshIdentityGroups(context.Background()); err != nil {
						t.Error(err)
						return
					}
					probeCtx, probeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
					defer probeCancel()
					concurrent := task
					concurrent.ControlAccountID = 0
					result, err := investigator.NewProbeExecutor(peer, a.gateway, a.logger).Execute(probeCtx, concurrent)
					if !errors.Is(err, context.DeadlineExceeded) || result.Outcome != model.ProbeResultError || result.Detail != "worker_interrupted" {
						t.Errorf("linked concurrent measurement was not bounded: %+v err=%v", result, err)
					}
				}
				if (scenario == "epoch_read_failed_after_primary" && n == 1) || (scenario == "epoch_read_failed_after_control" && n == 2) {
					hideEpochs()
				}
				if (scenario == "stale_inflight" && n == 1) || (scenario == "control_stale_inflight" && n == 2) {
					advance()
				}
				if (scenario == "cancel_primary" && n == 1) || (scenario == "cancel_control" && n == 2) {
					cancel()
				}
			}
			if err := a.qualityInvestigator.RunDue(ctx, a.qualityProbeExec, 1); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "epoch_read_failed") {
				if err := peer.DB().Migrator().RenameTable("e08_hidden_epoch", "q_node_epoch"); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := tasks.ListProbeTasksForCase(context.Background(), caseID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
			row := rows[0]
			wantCalls := int32(2)
			wantState := model.ProbeFailed
			wantResult := model.ProbeResultError
			wantVerified := false
			switch scenario {
			case "clean", "legacy_baseline", "identity_changed", "incomplete":
				wantCalls = 1
				wantState = model.ProbeDone
				wantResult = model.ProbeResultClean
			case "degraded_with_control":
				wantState = model.ProbeDone
				wantResult = model.ProbeResultDegraded
				wantVerified = true
			case "server_error_with_control":
				wantVerified = true
			case "stale_before", "policy_changed", "epoch_read_failed_before":
				wantCalls = 0
			case "stale_inflight", "epoch_read_failed_after_primary":
				wantCalls = 1
			case "cancel_primary":
				wantCalls = 1
				wantState = model.ProbeCancelled
			case "cancel_control":
				wantState = model.ProbeCancelled
			}
			if jury && scenario == "control_stale_inflight" {
				wantState = model.ProbeDone
				wantResult = model.ProbeResultDegraded
			}
			if calls.Load() != wantCalls || row.State != wantState || row.Result != wantResult || row.ControlVerified != wantVerified {
				t.Fatalf("calls=%d row=%+v", calls.Load(), row)
			}
			subject, primaryNode := ids[0], nodes[1]
			if jury {
				subject, primaryNode = ids[1], nodes[0]
			}
			if wantCalls > 0 && (row.Attempt.ID == "" || row.Attempt.AccountID != subject || row.Attempt.Path.NodeID != primaryNode || !spec.Matches(row.Attempt)) {
				t.Fatalf("primary fact=%+v", row.Attempt)
			}
			if wantCalls > 1 && (row.ControlAttempt.ID == "" || row.ControlAttempt.ID == row.Attempt.ID || row.ControlAttempt.AccountID != ids[1]) {
				t.Fatalf("control fact=%+v", row.ControlAttempt)
			}
			var exchanges int64
			if err := a.quality.DB().Table("q_guard_event").Where("stage = ?", "exchange").Count(&exchanges).Error; err != nil {
				t.Fatal(err)
			}
			if exchanges != int64(wantCalls) {
				t.Fatalf("physical receipts=%d calls=%d", exchanges, wantCalls)
			}
			if strings.Contains(row.Detail, "synthetic upstream failure") {
				t.Fatal("raw provider text persisted")
			}
			views, err := a.qualityCourt.LiveCaseViews(context.Background(), time.Now())
			if err != nil || len(views) != 1 || views[0].Assessment == nil {
				t.Fatalf("views=%+v err=%v", views, err)
			}
			assessment := views[0].Assessment
			wantDegraded, wantTransport, wantClean := 0, 0, 0
			if scenario == "degraded_with_control" {
				wantDegraded = 1
			}
			if scenario == "server_error_with_control" {
				wantTransport = 1
			}
			if scenario == "clean" || scenario == "legacy_baseline" || scenario == "identity_changed" || scenario == "incomplete" {
				wantClean = 1
			}
			group := assessment.Account
			if jury {
				group = assessment.Exit
			}
			if group.ConfirmedDegraded != wantDegraded || group.ConfirmedTransport != wantTransport || group.Clean != wantClean {
				t.Fatalf("assessment=%+v", assessment)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
