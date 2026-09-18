package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestAccountCheckRunsPinnedCompletedSamplesWithoutReleasingCase(t *testing.T) {
	var mu sync.Mutex
	var sessions, prompts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fictional-account-check-token" {
			t.Error("wrong account was used")
		}
		var body struct {
			Input string `json:"input"`
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		sessions = append(sessions, r.Header.Get("x-grok-session-id"))
		prompts = append(prompts, body.Input)
		mu.Unlock()
		count := 200
		if len(body.Input) > 100 {
			count = 263
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"fictional-response\",\"status\":\"in_progress\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"fictional thinking\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fictional-response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":10,\"output_tokens_details\":{\"reasoning_tokens\":8}}}}\n\n", count)
	}))
	defer upstream.Close()
	a := newLifecycleApplication(t, func(cfg *config.Config) {
		cfg.Provider.Build.BaseURL = upstream.URL + "/v1"
		cfg.Provider.Build.FallbackBaseURL = "disabled"
	})
	ctx := context.Background()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("fictional-account-check-token")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "fictional-quality-check", SourceKey: "fictional-quality-check", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, credential.ID, []string{"grok-4.6"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	caseID, err := a.quality.OpenInvestigation(ctx, credential.ID, model.EpochKey{NodeID: 991}, time.Now().UTC(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	before := a.quality.AccountState(credential.ID)
	store := registry.NewProbeTaskStore(a.quality)
	checks := management.NewAccountChecks(store, a.gateway)
	id, err := checks.Start(ctx, credential.ID, "grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.qualityInvestigator.RunDueOnce(ctx, a.qualityProbeExec, 1); err != nil {
		t.Fatalf("worker: %v", err)
	}
	rows, err := checks.List(ctx, credential.ID)
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].State != model.ProbeDone || rows[0].Report == nil {
		t.Fatalf("report: %+v %v", rows, err)
	}
	report := rows[0].Report
	if report.Outcome != "clean" || report.Fingerprint == nil || !report.Fingerprint.Stable || report.Fingerprint.RepeatDelta != 63 {
		t.Fatalf("lost full-stream evidence: %+v", report)
	}
	if got := a.quality.AccountState(credential.ID); got.State != before.State {
		t.Fatalf("manual check released case %d: %+v", caseID, got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 3 || sessions[0] == "" || sessions[0] == sessions[1] || sessions[1] == sessions[2] || sessions[0] == sessions[2] || prompts[0] != prompts[2] || prompts[0] == prompts[1] {
		t.Fatalf("incorrect independent samples: sessions=%v prompts=%d", sessions, len(prompts))
	}
	seen := map[string]bool{}
	for _, sample := range report.Samples {
		if sample.Attempt.AccountID != credential.ID || sample.Attempt.ID == "" || seen[sample.Attempt.ID] || !sample.Completed || !sample.UsageReported {
			t.Fatalf("unverified sample: %+v", sample)
		}
		seen[sample.Attempt.ID] = true
	}
}
