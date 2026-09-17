package inference

import (
	"context"
	"encoding/json"
	"errors"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestVideoWorkerKeepsAcceptedAccountScope(t *testing.T) {
	for _, stage := range []string{"account_disabled", "account_tier_changed", "create_rate_limited", "key_scope_widened", "same_tier_retry", "key_disabled", "public_model_renamed", "recovery_scope"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var calls, transport atomic.Int32
			upstream := voiceCompletionUpstream(t, &transport, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/rest/app-chat/conversations/new" {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				count := calls.Add(1)
				if (stage == "create_rate_limited" || stage == "same_tier_retry") && count == 1 {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"message":"video quota exhausted"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
			})
			defer upstream.Close()
			fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-video", account.ProviderWeb, nil)
			key := fx.created.Key
			key.ProviderScope, key.TierScope = clientkey.ProviderScopeWeb, clientkey.TierScopeFree
			if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{ProviderScope: &key.ProviderScope, TierScope: &key.TierScope}); err != nil {
				t.Fatal(err)
			}
			fx.account.Priority = 200
			if _, err := fx.accounts.UpdateAdministration(ctx, fx.account.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Priority: &fx.account.Priority}}); err != nil {
				t.Fatal(err)
			}
			second := fx.account
			second.ID, second.SourceKey, second.Name = 0, "video-super", "video-super"
			second.UserID = "597f19f8-49d4-458a-bee4-43ec3dcaf8ca"
			second.WebTier = account.WebTierSuper
			second.Priority = 50
			if stage == "same_tier_retry" {
				second.WebTier = account.WebTierBasic
			}
			second, _, err := fx.accounts.UpsertByIdentity(ctx, second)
			if err != nil || second.ID == fx.account.ID {
				t.Fatalf("second account=%+v err=%v", second, err)
			}
			if err := testsupport.Capabilities(ctx, fx.models, fx.accounts, second.ID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
			fx.service.UpdateVideoMaxAttempts(3)
			job := createVideoAuthorizationJob(t, fx)
			if job.AccountID != fx.account.ID {
				t.Fatalf("HTTP admission ignored free-only scope: account=%d", job.AccountID)
			}
			if stage == "account_disabled" || stage == "account_tier_changed" || stage == "key_scope_widened" || stage == "recovery_scope" {
				first := fx.account
				if stage == "account_tier_changed" {
					first.WebTier = account.WebTierSuper
				} else {
					first.Enabled = false
					if _, err := fx.accounts.UpdateAdministration(ctx, first.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &first.Enabled}}); err != nil {
						t.Fatal(err)
					}
				}
				if _, _, err := fx.accounts.UpsertByIdentity(ctx, first); err != nil {
					t.Fatal(err)
				}
				fx.selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderWeb, AccountID: first.ID})
			}
			if stage == "key_scope_widened" {
				key.TierScope = clientkey.TierScopeAll
				if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{TierScope: &key.TierScope}); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "key_disabled" {
				key.Enabled = false
				if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{Enabled: &key.Enabled}); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "public_model_renamed" {
				route, err := fx.models.Get(ctx, job.ModelRouteID)
				if err != nil {
					t.Fatal(err)
				}
				route.PublicID = "video-renamed-after-acceptance"
				if _, err := fx.models.Patch(ctx, route.ID, model.RoutePatch{PublicID: &route.PublicID}); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "recovery_scope" {
				// Drop the local wakeup queue and reload the accepted task from SQL.
				fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
				if err := fx.service.RecoverVideoJobs(ctx); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			for {
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, key.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.UsageRecordedAt != nil {
					job = stored
					break
				}
				select {
				case <-time.After(time.Millisecond):
				case <-ctx.Done():
					t.Fatal("video worker did not finish")
				}
			}
			cancel()
			<-done
			wantCalls := int32(0)
			wantAccount := fx.account.ID
			if stage == "create_rate_limited" || stage == "key_disabled" || stage == "public_model_renamed" {
				wantCalls = 1
			}
			if stage == "same_tier_retry" {
				wantCalls, wantAccount = 2, second.ID
			}
			if calls.Load() != wantCalls || job.AccountID != wantAccount {
				t.Fatalf("accepted free-only request escaped account scope: calls=%d want=%d final_account=%d initial=%d other=%d", calls.Load(), wantCalls, job.AccountID, fx.account.ID, second.ID)
			}
			scope, err := job.AccessPolicy.Scope()
			if err != nil || scope.Providers != clientkey.ProviderScopeWeb || scope.Tiers != clientkey.TierScopeFree {
				t.Fatalf("accepted policy changed: %+v %v", job.AccessPolicy, err)
			}
			storedKey, err := fx.clients.Get(context.Background(), key.ID)
			if err != nil || storedKey.ReservedUsageUSDTicks != 0 {
				t.Fatalf("failed job kept reservation: %+v %v", storedKey, err)
			}
			for _, id := range []uint64{fx.account.ID, second.ID} {
				if count, _ := fx.concurrency.Current(context.Background(), repository.AccountConcurrencyKey(id)); count != 0 {
					t.Fatalf("account %d capacity remained=%d", id, count)
				}
			}
		})
	}
}

func createVideoAuthorizationJob(t *testing.T, fx voiceCompletionFixture) media.Job {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": fx.publicModel, "prompt": "test video", "duration": 5, "resolution": "720p"})
	request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	fx.router.ServeHTTP(response, request)
	var created struct {
		ID string `json:"request_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || response.Code != http.StatusOK || created.ID == "" {
		t.Fatalf("create: status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	job, err := fx.jobs.GetMediaJob(context.Background(), created.ID, fx.created.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

type videoPolicyWriteFailure struct{ repository.MediaJobRepository }

func (videoPolicyWriteFailure) SaveMediaJobAccessPolicy(context.Context, string, string, media.JobAccessPolicy) error {
	return errors.New("injected authorization persistence failure")
}

func TestLegacyVideoWorkerRequiresExplicitPermission(t *testing.T) {
	for _, stage := range []string{"permitted", "key_disabled", "key_expired", "provider_denied", "model_denied", "model_restricted_empty", "policy_write_failed"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var calls, transport atomic.Int32
			upstream := voiceCompletionUpstream(t, &transport, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/rest/app-chat/conversations/new" {
					calls.Add(1)
					_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			})
			defer upstream.Close()
			fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-video", account.ProviderWeb, nil)
			key := fx.created.Key
			key.ProviderScope, key.TierScope = clientkey.ProviderScopeWeb, clientkey.TierScopeFree
			switch stage {
			case "key_disabled":
				key.Enabled = false
			case "key_expired":
				expired := time.Now().Add(-time.Hour)
				key.ExpiresAt = &expired
			case "provider_denied":
				key.ProviderScope = clientkey.ProviderScopeBuild
			case "model_denied":
				if err := fx.models.ReplaceProviderRoutes(ctx, account.ProviderWeb, model.CatalogRoutes(account.ProviderWeb)); err != nil {
					t.Fatal(err)
				}
				if err := testsupport.Capabilities(ctx, fx.models, fx.accounts, fx.account.ID, []string{"grok-imagine-video", "grok-chat-fast"}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				other, err := fx.models.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-chat-fast")
				if err != nil {
					t.Fatal(err)
				}
				key.AllowedModels = []uint64{other.ID}
			}
			patch := clientkey.ManagementPatch{ProviderScope: &key.ProviderScope, TierScope: &key.TierScope, Enabled: &key.Enabled, ExpiresAt: key.ExpiresAt, AllowedModels: &key.AllowedModels}
			if stage == "model_restricted_empty" {
				scope := clientkey.ModelScopeRestricted
				patch.ModelScope = &scope
			}
			if _, err := fx.clients.Patch(ctx, key.ID, patch); err != nil {
				t.Fatal(err)
			}
			route, err := fx.models.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			job := media.Job{ID: "video_legacy_" + stage, RequestID: "legacy-" + stage,
				ClientKeyID: key.ID, ClientKeyName: key.Name, AccountID: fx.account.ID, AccountName: fx.account.Name,
				Provider: string(account.ProviderWeb), Model: fx.publicModel, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
				Prompt: "legacy video", Seconds: 5, Quality: "720p", Status: media.StatusQueued, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now}
			if err := fx.jobs.CreateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			var jobs repository.MediaJobRepository = fx.jobs
			if stage == "policy_write_failed" {
				jobs = videoPolicyWriteFailure{MediaJobRepository: fx.jobs}
			}
			fx.service.ConfigureMedia(jobs, mediaapp.NewVideoResources(jobs, nil), 1)
			if err := fx.service.RecoverVideoJobs(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			for {
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, key.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.UsageRecordedAt != nil {
					job = stored
					break
				}
				select {
				case <-time.After(time.Millisecond):
				case <-ctx.Done():
					t.Fatal("legacy video worker did not finish")
				}
			}
			cancel()
			<-done
			if stage == "permitted" {
				scope, err := job.AccessPolicy.Scope()
				if err != nil || scope.Tiers != clientkey.TierScopeFree || scope.Providers != clientkey.ProviderScopeWeb || calls.Load() != 1 {
					t.Fatalf("legacy grant not adopted: policy=%+v calls=%d err=%v", job.AccessPolicy, calls.Load(), err)
				}
			} else if calls.Load() != 0 || !job.AccessPolicy.IsLegacy() || job.ErrorCode != "authorization_unavailable" {
				t.Fatalf("legacy job executed without permission: policy=%+v calls=%d code=%s", job.AccessPolicy, calls.Load(), job.ErrorCode)
			}
		})
	}
}
