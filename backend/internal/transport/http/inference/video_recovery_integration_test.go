package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	mediahttp "github.com/chenyme/grok2api/backend/internal/transport/http/media"
	"github.com/gin-gonic/gin"
)

// Recovery goes through the same SQL claim and actual Provider as the original
// worker. The test advances only the stored lease after the first worker drains.
func TestVideoRecoveryDoesNotCreateAnotherGeneration(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		operations := []media.VideoOperation{media.VideoOperationGenerate}
		if kind == account.ProviderConsole {
			operations = append(operations, media.VideoOperationEdit, media.VideoOperationExtend)
		}
		for _, operation := range operations {
			for _, blocked := range []bool{false, true} {
				name := string(kind) + "/" + string(operation) + "/available"
				if blocked {
					name = string(kind) + "/" + string(operation) + "/original_account_disabled"
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					var creates, polls, transport atomic.Int32
					var recovered atomic.Bool
					entered := make(chan struct{})
					releaseFirst := make(chan struct{})
					var once sync.Once
					upstream := voiceCompletionUpstream(t, &transport, func(w fhttp.ResponseWriter, r *fhttp.Request) {
						w.Header().Set("Content-Type", "application/json")
						if r.Method == http.MethodPost {
							if kind == account.ProviderConsole {
								path := "/v1/videos/generations"
								if operation == media.VideoOperationEdit {
									path = "/v1/videos/edits"
								} else if operation == media.VideoOperationExtend {
									path = "/v1/videos/extensions"
								}
								if r.URL.Path != path {
									t.Errorf("operation reached wrong native endpoint: %s want %s", r.URL.Path, path)
								}
							}
							creates.Add(1)
							if kind != account.ProviderWeb {
								_, _ = w.Write([]byte(`{"request_id":"original-native-video"}`))
								return
							}
						} else {
							polls.Add(1)
						}
						if !recovered.Load() {
							once.Do(func() { close(entered) })
							select {
							case <-r.Context().Done():
							case <-releaseFirst:
							case <-ctx.Done():
							}
							return
						}
						if kind == account.ProviderWeb {
							_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
						} else {
							if r.URL.Path != "/v1/videos/original-native-video" && r.URL.Path != "/videos/original-native-video" {
								t.Errorf("recovery queried another native identity: %s", r.URL.Path)
							}
							_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
						}
					})
					defer upstream.Close()
					model := "grok-imagine-video"
					if kind == account.ProviderBuild {
						model = "grok-imagine-video-1.5"
					}
					fx := newMediaCompletionFixture(t, upstream.URL, model, kind, nil)
					if kind == account.ProviderBuild {
						credential := fx.account
						credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
						if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
							t.Fatal(err)
						}
					}
					fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
					fx.service.UpdateVideoMaxAttempts(1)
					job := createVideoRecoveryJob(t, fx, operation)
					firstCtx, firstCancel := context.WithCancel(ctx)
					firstDone := make(chan struct{})
					go func() { fx.service.RunVideoWorkers(firstCtx); close(firstDone) }()
					defer func() { firstCancel(); <-firstDone }()
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal("first submission not observed")
					}
					firstCancel()
					select {
					case <-firstDone:
					case <-ctx.Done():
						t.Fatal("first worker did not drain")
					}
					stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
					if err != nil {
						t.Fatal(err)
					}
					if stored.Status != media.StatusInProgress {
						t.Fatalf("canceled worker cannot recover: status=%s", stored.Status)
					}
					close(releaseFirst)
					expired := time.Now().UTC().Add(-time.Second)
					stored.LeaseUntil = &expired
					if err := fx.jobs.UpdateMediaJob(ctx, stored); err != nil {
						t.Fatal(err)
					}
					if blocked {
						original, err := fx.accounts.Get(ctx, fx.account.ID)
						if err != nil {
							t.Fatal(err)
						}
						alternative := original
						alternative.ID = 0
						alternative.SourceKey = "native-alternative"
						alternative.Name = "native-alternative"
						alternative.UserID = "697f19f8-49d4-458a-bee4-43ec3dcaf8ca"
						alternative, _, err = fx.accounts.UpsertByIdentity(ctx, alternative)
						if err != nil {
							t.Fatal(err)
						}
						if err := testsupport.Capabilities(ctx, fx.models, fx.accounts, alternative.ID, []string{model}, time.Now().UTC()); err != nil {
							t.Fatal(err)
						}
						original.Enabled = false
						if _, err := fx.accounts.UpdateAdministration(ctx, original.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &original.Enabled}}); err != nil {
							t.Fatal(err)
						}
						fx.selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: kind, AccountID: original.ID})
					}
					recovered.Store(true)
					recoverable, recoverErr := fx.jobs.ListRecoverableMediaJobs(ctx, 10)
					if recoverErr != nil || len(recoverable) != 1 {
						t.Fatalf("expired task was not recoverable: values=%+v err=%v stored=%+v", recoverable, recoverErr, stored)
					}
					fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
					if err := fx.service.RecoverVideoJobs(ctx); err != nil {
						t.Fatal(err)
					}
					secondCtx, secondCancel := context.WithCancel(ctx)
					secondDone := make(chan struct{})
					go func() { fx.service.RunVideoWorkers(secondCtx); close(secondDone) }()
					defer func() { secondCancel(); <-secondDone }()
					for {
						stored, err = fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
						if err != nil {
							t.Fatal(err)
						}
						if stored.UsageRecordedAt != nil {
							break
						}
						select {
						case <-time.After(time.Millisecond):
						case <-ctx.Done():
							t.Fatalf("recovered worker did not finalize: creates=%d polls=%d status=%s lease=%v claim=%s error=%s", creates.Load(), polls.Load(), stored.Status, stored.LeaseUntil, stored.ClaimToken, stored.ErrorCode)
						}
					}
					secondCancel()
					<-secondDone
					if creates.Load() != 1 {
						t.Fatalf("recovery created another billable generation: creates=%d polls=%d", creates.Load(), polls.Load())
					}
					wantPolls := int32(2)
					if blocked {
						wantPolls = 1
					}
					if stored.AccountID != fx.account.ID {
						t.Fatal("recovery switched the native account")
					}
					if kind != account.ProviderWeb && blocked && stored.ErrorCode != "account_unavailable" {
						t.Fatalf("disabled native account was not rejected: %s", stored.ErrorCode)
					}
					if kind != account.ProviderWeb && polls.Load() != wantPolls {
						t.Fatalf("did not resume original polling: polls=%d", polls.Load())
					}
					if operation != media.VideoOperationGenerate && !blocked {
						record := waitVoiceAudit(t, fx.audits)
						assertVideoBilling(t, fx, record)
						if record.GenerationOutcome != "completed" || record.MediaOutputSeconds != 0 || record.EstimatedCostInUSDTicks != 0 {
							t.Fatalf("edit/extend invented output duration or price: %+v", record)
						}
					}
					if kind == account.ProviderWeb && stored.ErrorCode != "generation_unconfirmed" {
						t.Fatalf("unqueryable submitted Web generation falsely classified: %s", stored.ErrorCode)
					}
				})
			}
		}
	}
}

func TestVideoArchiveFailureRetainsKnownGeneration(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var transport atomic.Int32
			upstream := voiceCompletionUpstream(t, &transport, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				if kind != account.ProviderWeb && r.Method == http.MethodPost {
					_, _ = w.Write([]byte(`{"request_id":"archive-failed-native"}`))
					return
				}
				if kind == account.ProviderWeb {
					_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
				} else {
					_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
				}
			})
			defer upstream.Close()
			model := "grok-imagine-video"
			if kind == account.ProviderBuild {
				model = "grok-imagine-video-1.5"
			}
			fx := newMediaCompletionFixture(t, upstream.URL, model, kind, nil)
			if kind == account.ProviderBuild {
				credential := fx.account
				credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
				if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
					t.Fatal(err)
				}
			}
			fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
			fx.service.UpdateVideoMaxAttempts(1)
			job := createVideoAuthorizationJob(t, fx)
			done := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			record := waitVoiceAudit(t, fx.audits)
			assertVideoBilling(t, fx, record)
			stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != media.StatusFailed {
				t.Fatalf("expected actual storage failure, status=%s", stored.Status)
			}
			if stored.UpstreamURL == "" || record.GenerationOutcome != "completed" || record.MediaOutputSeconds != 5 || record.EstimatedCostInUSDTicks <= 0 {
				t.Fatalf("archive failure erased known generation: result=%q generation=%s seconds=%d cost=%d", stored.UpstreamURL, record.GenerationOutcome, record.MediaOutputSeconds, record.EstimatedCostInUSDTicks)
			}
		})
	}
}

func TestXAIVideoRecoveryKeepsNativeJobAndUpload(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "direct"
		if fallback {
			name = "primary_403_fallback"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var creates, primary, polls, transport atomic.Int32
			uploadURL := make(chan string, 1)
			upstream := voiceCompletionUpstream(t, &transport, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					if strings.HasPrefix(r.URL.Path, "/v1/") {
						primary.Add(1)
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"error":"forbidden"}`))
						return
					}
					creates.Add(1)
					if r.URL.Path != "/xai/videos/generations" {
						t.Errorf("unexpected creation route %s", r.URL.Path)
					}
					var payload struct {
						Output struct {
							UploadURL string `json:"upload_url"`
						} `json:"output"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					select {
					case uploadURL <- payload.Output.UploadURL:
					default:
					}
					_, _ = w.Write([]byte(`{"request_id":"xai-native-original"}`))
					return
				}
				polls.Add(1)
				if r.URL.Path != "/xai/videos/xai-native-original" {
					t.Errorf("lost native route or ID: %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"status":"done","video":{}}`))
			})
			defer upstream.Close()
			var build *cli.Adapter
			fx := newProviderCompletionFixture(t, upstream.URL, "grok-imagine-video-1.5", account.ProviderBuild, func(store provider.ImageAssetStore) provider.ImageAssetStore { return store }, func(adapter provider.Adapter) provider.Adapter { build = adapter.(*cli.Adapter); return adapter })
			mediaRouter := gin.New()
			mediahttp.NewHandler(fx.media, nil).RegisterPublic(mediaRouter.Group("/v1/media"))
			uploadServer := httptest.NewTLSServer(mediaRouter)
			defer uploadServer.Close()
			fx.media.UpdateConfig(mediaapp.Config{PublicBaseURL: uploadServer.URL})
			build.SetVideoUploadIssuer(fx.media)
			build.UpdateConfig(cli.Config{BaseURL: upstream.URL + "/v1", FallbackBaseURL: upstream.URL + "/xai"})
			credential := fx.account
			credential.BuildSuperEntitled = true
			credential.BuildRouteMode = account.BuildRouteXAI
			if fallback {
				credential.BuildRouteMode = account.BuildRouteAuto
			}
			if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
				t.Fatal(err)
			}
			fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
			fx.service.ConfigureMediaAssets(fx.media)
			job := createVideoAuthorizationJob(t, fx)
			firstCtx, firstCancel := context.WithCancel(ctx)
			firstDone := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(firstCtx); close(firstDone) }()
			defer func() { firstCancel(); <-firstDone }()
			var target string
			select {
			case target = <-uploadURL:
			case <-ctx.Done():
				t.Fatal("XAI submission never arrived")
			}
			for {
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Execution.Phase == media.VideoExecutionGenerated {
					job = stored
					break
				}
				select {
				case <-time.After(time.Millisecond):
				case <-ctx.Done():
					t.Fatal("generated checkpoint never saved")
				}
			}
			if job.Execution.NativeJobID != "xai-native-original" || job.Execution.Route != "xai" || job.Execution.UploadAssetID == "" || job.ResultAssetID != "" {
				t.Fatalf("pending upload confused with ready asset: %+v", job)
			}
			firstCancel()
			<-firstDone
			payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 64)...)
			request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "video/mp4")
			response, err := uploadServer.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
				t.Fatalf("actual PUT failed: %d", response.StatusCode)
			}
			job, err = fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
			if err != nil {
				t.Fatal(err)
			}
			if job.ResultAssetID != "" {
				t.Fatal("late upload bypassed worker checkpoint")
			}
			expired := time.Now().UTC().Add(-time.Second)
			job.LeaseUntil = &expired
			if err := fx.jobs.UpdateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			// A configuration and account route preference edit cannot move the native job.
			build.UpdateConfig(cli.Config{BaseURL: upstream.URL + "/moved-build", FallbackBaseURL: upstream.URL + "/moved-xai"})
			credential.BuildRouteMode = account.BuildRouteBuild
			if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
				t.Fatal(err)
			}
			fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
			if err := fx.service.RecoverVideoJobs(ctx); err != nil {
				t.Fatal(err)
			}
			secondCtx, secondCancel := context.WithCancel(ctx)
			secondDone := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(secondCtx); close(secondDone) }()
			defer func() { secondCancel(); <-secondDone }()
			for {
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
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
					t.Fatal("uploaded native job did not recover")
				}
			}
			secondCancel()
			<-secondDone
			wantPrimary := int32(0)
			if fallback {
				wantPrimary = 1
			}
			if creates.Load() != 1 || primary.Load() != wantPrimary || polls.Load() != 2 || job.Status != media.StatusCompleted || job.ResultAssetID != job.Execution.UploadAssetID {
				t.Fatalf("native/upload recovery failed: creates=%d primary=%d polls=%d job=%+v", creates.Load(), primary.Load(), polls.Load(), job)
			}
			_, body, err := fx.media.OpenVideo(ctx, job.ResultAssetID)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(body)
			_ = body.Close()
			if err != nil || !bytes.Equal(data, payload) {
				t.Fatalf("recovered asset differs: %v", err)
			}
			record := waitVoiceAudit(t, fx.audits)
			assertVideoBilling(t, fx, record)
			fx.receipts.mu.Lock()
			facts := append(fx.receipts.facts[:0:0], fx.receipts.facts...)
			fx.receipts.mu.Unlock()
			wantPhysical := 3 + int(wantPrimary)
			if len(facts) != wantPhysical || job.Limits.Reserved != uint32(wantPhysical) || job.Limits.Confirmed != uint32(wantPhysical) || record.PhysicalReceipt != "committed" {
				t.Fatalf("XAI physical accounting: facts=%d limits=%+v receipt=%s", len(facts), job.Limits, record.PhysicalReceipt)
			}
			for i, fact := range facts {
				plane := "xai"
				if fallback && i == 0 {
					plane = "build"
				}
				if fact.Plane != plane || fact.Attempt.AccountID != fx.account.ID {
					t.Fatalf("XAI physical identity/plane differs: %+v", fact)
				}
			}

			if record.GenerationOutcome != "completed" || record.MediaOutputSeconds != 5 || record.EstimatedCostInUSDTicks <= 0 {
				t.Fatalf("recovered generated facts lost: %+v", record)
			}
		})
	}
}

type videoExecutionFaults struct {
	repository.MediaJobRepository
	phase    media.VideoExecutionPhase
	terminal bool
	failures atomic.Int32
	reached  chan struct{}
	once     sync.Once
}

func (r *videoExecutionFaults) failure() error {
	if r.failures.Add(1) >= 3 {
		r.once.Do(func() { close(r.reached) })
	}
	return errors.New("injected video persistence failure")
}
func (r *videoExecutionFaults) SaveMediaJobExecution(ctx context.Context, job media.Job, previous media.VideoExecution) error {
	if job.Execution.Phase == r.phase {
		return r.failure()
	}
	return r.MediaJobRepository.SaveMediaJobExecution(ctx, job, previous)
}
func (r *videoExecutionFaults) UpdateMediaJob(ctx context.Context, job media.Job) error {
	if r.terminal && (job.Status == media.StatusCompleted || job.Status == media.StatusFailed) {
		return r.failure()
	}
	return r.MediaJobRepository.UpdateMediaJob(ctx, job)
}

func TestVideoRecoveryAfterCheckpointWriteFailure(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		for _, stage := range []string{"before_submit", "native_id", "generated", "terminal", "failed_terminal"} {
			if kind == account.ProviderWeb && stage == "native_id" {
				continue
			}
			t.Run(string(kind)+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var creates, polls, transport atomic.Int32
				upstream := voiceCompletionUpstream(t, &transport, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost {
						creates.Add(1)
						if kind != account.ProviderWeb {
							_, _ = w.Write([]byte(`{"request_id":"native-persistence-failure"}`))
							return
						}
					} else {
						polls.Add(1)
					}
					if stage == "failed_terminal" {
						_, _ = w.Write([]byte(`{"status":"failed","error":{"message":"native failed before shutdown"}}`))
						return
					}
					if kind == account.ProviderWeb {
						_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
					} else {
						_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
					}
				})
				defer upstream.Close()
				model := "grok-imagine-video"
				if kind == account.ProviderBuild {
					model = "grok-imagine-video-1.5"
				}
				fx := newMediaCompletionFixture(t, upstream.URL, model, kind, nil)
				quotaMode := ""
				if kind == account.ProviderConsole {
					quotaMode = console.QuotaModeVideo
				} else if kind == account.ProviderWeb {
					quotaMode = account.QuotaModeWebVideo720p
				}
				if quotaMode != "" {
					if err := saveQuotaWindowsFixture(fx.accounts, ctx, fx.account.ID, account.WebTierBasic, time.Now().UTC(), []account.QuotaWindow{{Mode: quotaMode, Remaining: 20, Total: 20}}); err != nil {
						t.Fatal(err)
					}
				}
				if kind == account.ProviderBuild {
					credential := fx.account
					credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
					if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
						t.Fatal(err)
					}
				}
				fault := &videoExecutionFaults{MediaJobRepository: fx.jobs, reached: make(chan struct{})}
				switch stage {
				case "before_submit":
					fault.phase = media.VideoExecutionSubmitting
				case "native_id":
					fault.phase = media.VideoExecutionSubmitted
				case "generated":
					fault.phase = media.VideoExecutionGenerated
				case "terminal", "failed_terminal":
					fault.terminal = true
				}
				fx.service.ConfigureMedia(fault, mediaapp.NewVideoResources(fault, nil), 1)
				fx.service.UpdateVideoMaxAttempts(3)
				job := createVideoAuthorizationJob(t, fx)
				firstCtx, firstCancel := context.WithCancel(ctx)
				firstDone := make(chan struct{})
				go func() { fx.service.RunVideoWorkers(firstCtx); close(firstDone) }()
				defer func() { firstCancel(); <-firstDone }()
				select {
				case <-fault.reached:
				case <-ctx.Done():
					t.Fatal("expected persistence fault not reached")
				}
				firstCancel()
				<-firstDone
				before := int32(1)
				if stage == "before_submit" {
					before = 0
				}
				if creates.Load() != before {
					t.Fatalf("continued past unacknowledged checkpoint: creates=%d want=%d", creates.Load(), before)
				}
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status != media.StatusInProgress {
					t.Fatalf("failed checkpoint falsely finalized task: %+v", stored)
				}
				expired := time.Now().UTC().Add(-time.Second)
				stored.LeaseUntil = &expired
				if err := fx.jobs.UpdateMediaJob(ctx, stored); err != nil {
					t.Fatal(err)
				}
				fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
				if err := fx.service.RecoverVideoJobs(ctx); err != nil {
					t.Fatal(err)
				}
				secondCtx, secondCancel := context.WithCancel(ctx)
				secondDone := make(chan struct{})
				go func() { fx.service.RunVideoWorkers(secondCtx); close(secondDone) }()
				defer func() { secondCancel(); <-secondDone }()
				for {
					stored, err = fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
					if err != nil {
						t.Fatal(err)
					}
					if stored.UsageRecordedAt != nil {
						break
					}
					select {
					case <-time.After(time.Millisecond):
					case <-ctx.Done():
						t.Fatal("recovery did not finalize")
					}
				}
				secondCancel()
				<-secondDone
				if creates.Load() != 1 {
					t.Fatalf("persistence recovery duplicated generation: %d", creates.Load())
				}
				unknown := stage == "native_id" || (kind == account.ProviderWeb && stage == "generated")
				record := waitVoiceAudit(t, fx.audits)
				assertVideoBilling(t, fx, record)
				if quotaMode != "" {
					windows, err := fx.accounts.GetQuotaWindows(ctx, []uint64{fx.account.ID})
					if err != nil {
						t.Fatal(err)
					}
					want := 20
					if record.GenerationOutcome == "completed" {
						want = 19
					}
					if len(windows[fx.account.ID]) != 1 || windows[fx.account.ID][0].Remaining != want {
						t.Fatalf("quota after recovery/native fact = %+v expected=%d generation=%s", windows, want, record.GenerationOutcome)
					}
				}
				if unknown {
					if stored.ErrorCode != "generation_unconfirmed" || record.GenerationOutcome != "unconfirmed" {
						t.Fatalf("missing acknowledgement treated as known: code=%s generation=%s", stored.ErrorCode, record.GenerationOutcome)
					}
				} else if stage == "failed_terminal" {
					if stored.Execution.Phase != media.VideoExecutionFailed || record.GenerationOutcome != "failed" || record.EstimatedCostInUSDTicks != 0 {
						t.Fatalf("lost acknowledged native failure: job=%+v audit=%+v", stored, record)
					}
				} else if stored.Execution.Phase != media.VideoExecutionGenerated || record.GenerationOutcome != "completed" || record.EstimatedCostInUSDTicks <= 0 {
					t.Fatalf("acknowledged generation lost: job=%+v audit=%+v", stored, record)
				}
				if (stage == "terminal" || stage == "failed_terminal") && kind != account.ProviderWeb && polls.Load() != 1 {
					t.Fatalf("terminal retry repeated native polling: %d", polls.Load())
				}
			})
		}
	}
}

func TestLegacyInProgressVideoCannotBeAssumedUnsubmitted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var calls atomic.Int32
	upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
		t.Error("legacy in-progress task initiated another network call")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer upstream.Close()
	fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-video", account.ProviderWeb, nil)
	route, err := fx.models.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := media.Job{ID: "video_legacy_in_progress", RequestID: "legacy-unconfirmed", ClientKeyID: fx.created.Key.ID, ClientKeyName: fx.created.Key.Name, AccountID: fx.account.ID, AccountName: fx.account.Name, Provider: string(account.ProviderWeb), Model: fx.publicModel, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel, Prompt: "legacy accepted input", Seconds: 5, Quality: "720p", Status: media.StatusInProgress, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := fx.jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
	if err := fx.service.RecoverVideoJobs(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	record := waitVoiceAudit(t, fx.audits)
	assertVideoBilling(t, fx, record)
	if calls.Load() != 0 || record.GenerationOutcome != "unconfirmed" || record.EstimatedCostInUSDTicks != 0 {
		t.Fatalf("legacy result fabricated: calls=%d generation=%s cost=%d", calls.Load(), record.GenerationOutcome, record.EstimatedCostInUSDTicks)
	}
	stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
	if err != nil || stored.Execution.Phase != media.VideoExecutionUnconfirmed || stored.ErrorCode != "generation_unconfirmed" {
		t.Fatalf("legacy state not durable: %+v %v", stored, err)
	}
}

func createVideoRecoveryJob(t *testing.T, fx voiceCompletionFixture, operation media.VideoOperation) media.Job {
	t.Helper()
	if operation == media.VideoOperationGenerate {
		return createVideoAuthorizationJob(t, fx)
	}
	body := map[string]any{"model": fx.publicModel, "prompt": "edit or extend existing video", "video": map[string]any{"url": "https://vidgen.x.ai/input.mp4"}}
	path := "/v1/videos/edits"
	if operation == media.VideoOperationExtend {
		body["duration"] = 5
		path = "/v1/videos/extensions"
	}
	data, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	fx.router.ServeHTTP(response, request)
	var created struct {
		ID string `json:"request_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || response.Code != http.StatusOK || created.ID == "" {
		t.Fatalf("create %s failed: %d %s %v", operation, response.Code, response.Body.String(), err)
	}
	job, err := fx.jobs.GetMediaJob(context.Background(), created.ID, fx.created.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestVideoNativeTerminalFacts(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		for _, stage := range []string{"failed", "completed_without_url", "malformed"} {
			if kind == account.ProviderWeb && stage == "completed_without_url" {
				continue
			}
			t.Run(string(kind)+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var creates, transport atomic.Int32
				upstream := voiceCompletionUpstream(t, &transport, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost {
						creates.Add(1)
						if kind != account.ProviderWeb {
							_, _ = w.Write([]byte(`{"request_id":"native-terminal-fact"}`))
							return
						}
					}
					switch stage {
					case "failed":
						if kind == account.ProviderWeb {
							_, _ = w.Write([]byte(`{"error":{"message":"native generation failed"}}`))
						} else {
							_, _ = w.Write([]byte(`{"status":"failed","error":{"message":"native generation failed"}}`))
						}
					case "completed_without_url":
						_, _ = w.Write([]byte(`{"status":"done","video":{}}`))
					case "malformed":
						_, _ = w.Write([]byte(`{"status":`))
					}
				})
				defer upstream.Close()
				model := "grok-imagine-video"
				if kind == account.ProviderBuild {
					model = "grok-imagine-video-1.5"
				}
				fx := newMediaCompletionFixture(t, upstream.URL, model, kind, nil)
				quotaMode := ""
				if kind == account.ProviderConsole {
					quotaMode = console.QuotaModeVideo
				} else if kind == account.ProviderWeb {
					quotaMode = account.QuotaModeWebVideo720p
				}
				if quotaMode != "" {
					if err := saveQuotaWindowsFixture(fx.accounts, ctx, fx.account.ID, account.WebTierBasic, time.Now().UTC(), []account.QuotaWindow{{Mode: quotaMode, Remaining: 20, Total: 20}}); err != nil {
						t.Fatal(err)
					}
				}
				if kind == account.ProviderBuild {
					credential := fx.account
					credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
					if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
						t.Fatal(err)
					}
				}
				fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
				fx.service.UpdateVideoMaxAttempts(3)
				job := createVideoAuthorizationJob(t, fx)
				done := make(chan struct{})
				go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
				defer func() { cancel(); <-done }()
				record := waitVoiceAudit(t, fx.audits)
				assertVideoBilling(t, fx, record)
				if quotaMode != "" {
					windows, err := fx.accounts.GetQuotaWindows(ctx, []uint64{fx.account.ID})
					if err != nil {
						t.Fatal(err)
					}
					want := 20
					if record.GenerationOutcome == "completed" {
						want = 19
					}
					if len(windows[fx.account.ID]) != 1 || windows[fx.account.ID][0].Remaining != want {
						t.Fatalf("quota after recovery/native fact = %+v expected=%d generation=%s", windows, want, record.GenerationOutcome)
					}
				}
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
				if err != nil {
					t.Fatal(err)
				}
				if creates.Load() != 1 || stored.Status != media.StatusFailed || record.StatusCode != http.StatusBadGateway {
					t.Fatalf("native terminal handling changed execution: creates=%d status=%s audit_status=%d", creates.Load(), stored.Status, record.StatusCode)
				}
				wantGeneration := "unconfirmed"
				if stage == "failed" {
					wantGeneration = "failed"
				} else if stage == "completed_without_url" {
					wantGeneration = "completed"
				}
				if record.GenerationOutcome != wantGeneration {
					t.Fatalf("lost native terminal fact: generation=%s want=%s", record.GenerationOutcome, wantGeneration)
				}
				if stage == "completed_without_url" {
					if record.MediaOutputSeconds != 5 || record.EstimatedCostInUSDTicks <= 0 || stored.Execution.Phase != media.VideoExecutionGenerated {
						t.Fatalf("missing output erased known generation: %+v", record)
					}
				} else if record.MediaOutputSeconds != 0 || record.EstimatedCostInUSDTicks != 0 {
					t.Fatalf("unknown/failed generation invented output: %+v", record)
				}
			})
		}
	}
}

func assertVideoBilling(t *testing.T, fx voiceCompletionFixture, record audit.Record) {
	t.Helper()
	key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
	if err != nil || key.BilledUsageUSDTicks != record.EstimatedCostInUSDTicks || key.ReservedUsageUSDTicks != 0 {
		t.Fatalf("video billing diverged from durable fact: key=%+v cost=%d err=%v", key, record.EstimatedCostInUSDTicks, err)
	}
	_, count, err := fx.audits.List(context.Background(), 0, 10)
	if err != nil || count != 1 {
		t.Fatalf("recovery recorded logical usage multiple times: %d %v", count, err)
	}
}
