package inference

import (
	"context"
	"fmt"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestHTTPRejectsLegacyUnsupportedModelCapabilityBeforeUpstream(t *testing.T) {
	for _, tc := range []struct {
		kind  account.Provider
		up    string
		cap   model.Capability
		paths []string
	}{
		{account.ProviderWeb, "grok-chat-fast", model.CapabilityVideo, []string{"videos/generations"}},
		{account.ProviderWeb, "grok-imagine-image", model.CapabilityChat, []string{"responses", "chat/completions", "messages"}},
		{account.ProviderConsole, "grok-4.3", model.CapabilityImage, []string{"images/generations"}},
		{account.ProviderConsole, "grok-imagine-image", model.CapabilityResponses, []string{"responses", "chat/completions", "messages"}},
		{account.ProviderConsole, "grok-voice-latest", model.CapabilityVideo, []string{"videos/generations"}},
		{account.ProviderBuild, "future-text", model.CapabilityVideo, []string{"videos/generations"}},
		{account.ProviderBuild, model.BuildVideoModel, model.CapabilityResponses, []string{"responses", "chat/completions", "messages"}},
	} {
		t.Run(string(tc.kind)+"/"+tc.up+"/"+string(tc.cap), func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer upstream.Close()
			fx := newProviderCompletionFixture(t, upstream.URL, tc.up, tc.kind, nil, nil)
			fx.service.ConfigureMedia(fx.jobs, 1)
			// A literal Provider-prefixed public name must retain its invalid configured
			// meaning instead of falling through to a valid historical qualified name.
			_, err := fx.models.Create(ctx, model.Route{Provider: tc.kind, PublicID: tc.kind.ModelNamespace() + "/legacy", UpstreamModel: tc.up, Capability: map[account.Provider]model.Capability{account.ProviderBuild: model.CapabilityResponses, account.ProviderWeb: model.CapabilityChat, account.ProviderConsole: model.CapabilityResponses}[tc.kind], Enabled: true}, []uint64{fx.account.ID})
			if err != nil {
				t.Fatal(err)
			}
			bad, err := fx.models.Create(ctx, model.Route{Provider: tc.kind, PublicID: tc.kind.ModelNamespace() + "/" + tc.kind.ModelNamespace() + "/legacy", UpstreamModel: tc.up, Capability: tc.cap, Enabled: true}, []uint64{fx.account.ID})
			if err != nil {
				t.Fatal(err)
			}
			renamed := tc.kind.ModelNamespace() + "/renamed"
			if _, err = fx.models.Patch(ctx, bad.ID, model.RoutePatch{PublicID: &renamed}); err != nil {
				t.Fatal(err)
			}
			for _, path := range tc.paths {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream-%t", path, stream), func(t *testing.T) {
						body := fmt.Sprintf(`{"model":%q,"input":"synthetic","messages":[{"role":"user","content":"synthetic"}],"prompt":"synthetic","max_tokens":32,"duration":5,"stream":%t}`, tc.kind.ModelNamespace()+"/legacy", stream)
						if path == "videos/generations" {
							body = fmt.Sprintf(`{"model":%q,"prompt":"synthetic","duration":5}`, tc.kind.ModelNamespace()+"/legacy")
						}
						request := httptest.NewRequest(http.MethodPost, "/v1/"+path, strings.NewReader(body))
						request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Anthropic-Version", "2023-06-01")
						response := httptest.NewRecorder()
						fx.router.ServeHTTP(response, request)
						if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "model_unavailable") {
							t.Fatalf("unsupported request: %d %s", response.Code, response.Body.String())
						}
						if calls.Load() != 0 {
							t.Fatalf("unsupported capability made %d upstream calls", calls.Load())
						}
					})
				}
			}
			got, err := fx.models.Get(ctx, bad.ID)
			if err != nil || !got.Enabled || len(got.BoundAccountIDs) != 1 {
				t.Fatalf("legacy intent changed: %+v %v", got, err)
			}
			key, err := fx.clients.Get(ctx, fx.created.Key.ID)
			if err != nil || key.BilledUsageUSDTicks != 0 || key.ReservedUsageUSDTicks != 0 {
				t.Fatalf("invalid request changed billing: %+v %v", key, err)
			}
			_, total, err := fx.jobs.ListMediaJobs(ctx, repository.MediaJobListQuery{})
			if err != nil || total != 0 {
				t.Fatalf("invalid request created jobs: %d %v", total, err)
			}
		})
	}
}

// Queued legacy routes cannot create the fixed Build video product under a text
// name. An already submitted native job must still finish and record its cost.
func TestLegacyBuildVideoCapabilityPreservesSubmittedWork(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("submitted-%t", submitted), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var calls, creates atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if r.Method != http.MethodGet {
					creates.Add(1)
					w.WriteHeader(500)
					return
				}
				if r.URL.Path != "/v1/videos/old-native" {
					t.Errorf("wrong native identity: %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
			})
			defer upstream.Close()
			fx := newProviderCompletionFixture(t, upstream.URL, "future-text", account.ProviderBuild, nil, nil)
			yes := true
			mode := account.BuildRouteBuild
			if _, err := fx.accounts.UpdateAdministration(ctx, fx.account.ID, repository.AccountAdminPatch{BuildSuperEntitled: &yes, BuildRouteMode: &mode}); err != nil {
				t.Fatal(err)
			}
			route, err := fx.models.Create(ctx, model.Route{Provider: account.ProviderBuild, PublicID: "legacy-video", UpstreamModel: "future-text", Capability: model.CapabilityVideo, Enabled: true}, []uint64{fx.account.ID})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			job := media.Job{ID: "video_legacy_capability", RequestID: "legacy-capability", ClientKeyID: fx.created.Key.ID, ClientKeyName: fx.created.Key.Name, AccountID: fx.account.ID, AccountName: fx.account.Name, Provider: string(account.ProviderBuild), Model: "legacy-video", ModelRouteID: route.ID, UpstreamModel: "Build/future-text", Prompt: "synthetic", Seconds: 5, Quality: "720p", Status: media.StatusQueued, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now, Execution: media.VideoExecution{Revision: 1, Phase: media.VideoExecutionReady}}
			if submitted {
				job.Status = media.StatusInProgress
				job.Execution = media.VideoExecution{Revision: 2, Phase: media.VideoExecutionSubmitted, Route: "build", Endpoint: upstream.URL + "/v1", NativeJobID: "old-native"}
			}
			if err := fx.jobs.CreateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			fx.service.ConfigureMedia(fx.jobs, 1)
			if err := fx.service.RecoverVideoJobs(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			record := waitVoiceAudit(t, fx.audits)
			assertVideoBilling(t, fx, record)
			if creates.Load() != 0 {
				t.Fatalf("legacy created %d extra generations", creates.Load())
			}
			stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
			if err != nil {
				t.Fatal(err)
			}
			if submitted {
				if calls.Load() == 0 || record.GenerationOutcome != "completed" || record.EstimatedCostInUSDTicks <= 0 || stored.Execution.NativeJobID != "old-native" || stored.Execution.Phase != media.VideoExecutionGenerated || record.ModelUpstreamModel != "Build/future-text" {
					t.Fatalf("lost native generation/cost: record=%+v execution=%+v calls=%d", record, stored.Execution, calls.Load())
				}
			} else if calls.Load() != 0 || record.GenerationOutcome != "not_started" || record.EstimatedCostInUSDTicks != 0 || stored.ErrorCode != "unsupported_model" {
				t.Fatalf("queued invalid generation: record=%+v code=%s calls=%d", record, stored.ErrorCode, calls.Load())
			}
		})
	}
}
