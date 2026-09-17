package inference

import (
	"context"
	"encoding/json"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type videoLifecycleAdapter struct {
	provider.Adapter
	video    provider.VideoAdapter
	stage    string
	finished chan struct{}
	once     sync.Once
}

func (a *videoLifecycleAdapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	defer a.once.Do(func() { close(a.finished) })
	if a.stage == "credential_prepare" {
		request.Credential.EncryptedAccessToken = "invalid-ciphertext"
	}
	result, err := a.video.GenerateVideo(ctx, request)
	if a.stage == "panic_after_generation" {
		panic("injected after actual video generation")
	}
	return result, err
}

// Actual HTTP/DPoP adapters, persistent jobs, fixed workers and the real account
// limiter cooperate. Only panic and invalid credential preparation are injected
// at the adapter boundary; every other failure comes from the local upstream.
func TestVideoWorkerReleasesAccountOnEveryExit(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		for _, stage := range []string{"create_rejected", "poll_failed", "archive_failed", "cancel_after_submit", "credential_prepare", "panic_after_generation"} {
			t.Run(string(kind)+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var calls atomic.Int32
				entered := make(chan struct{})
				var once sync.Once
				upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					w.Header().Set("Content-Type", "application/json")
					isCreate := r.Method == http.MethodPost
					if isCreate && stage == "create_rejected" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"error":{"message":"content policy rejected"}}`))
						return
					}
					if kind != account.ProviderWeb && isCreate {
						_, _ = w.Write([]byte(`{"request_id":"remote-video-1"}`))
						return
					}
					if stage == "cancel_after_submit" {
						once.Do(func() { close(entered) })
						select {
						case <-r.Context().Done():
						case <-ctx.Done():
						}
						return
					}
					if stage == "poll_failed" {
						if kind == account.ProviderWeb {
							_, _ = w.Write([]byte(`{"error":{"message":"native stream failed"}}`))
						} else {
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = w.Write([]byte(`{"error":"poll unavailable"}`))
						}
						return
					}
					if kind == account.ProviderWeb {
						_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4","videoPostId":"remote-video-1"}}}}`))
					} else {
						_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
					}
				})
				defer upstream.Close()
				var wrapped *videoLifecycleAdapter
				model := "grok-imagine-video"
				if kind == account.ProviderBuild {
					model = "grok-imagine-video-1.5"
				}
				fx := newProviderCompletionFixture(t, upstream.URL, model, kind, nil, func(adapter provider.Adapter) provider.Adapter {
					wrapped = &videoLifecycleAdapter{Adapter: adapter, video: adapter.(provider.VideoAdapter), stage: stage, finished: make(chan struct{})}
					return wrapped
				})
				if kind == account.ProviderBuild {
					credential := fx.account
					credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
					if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
						t.Fatal(err)
					}
				}
				fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
				fx.service.UpdateVideoMaxAttempts(1)
				// Use the production HTTP admission, including the finite Key's
				// reservation, before starting the independent persistent worker.
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
				done := make(chan struct{})
				go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
				defer func() { cancel(); <-done }()
				if stage == "cancel_after_submit" || stage == "panic_after_generation" {
					observed := entered
					if stage == "panic_after_generation" {
						observed = wrapped.finished
					}
					select {
					case <-observed:
					case <-ctx.Done():
						t.Fatal("upstream never reached expected stage")
					}
				} else {
					for {
						job, err := fx.jobs.GetMediaJob(ctx, created.ID, fx.created.Key.ID)
						if err != nil {
							t.Fatal(err)
						}
						if job.UsageRecordedAt != nil {
							if job.Status != media.StatusFailed {
								t.Fatalf("expected failed stage, job=%+v", job)
							}
							break
						}
						select {
						case <-time.After(time.Millisecond):
						case <-ctx.Done():
							t.Fatal("worker did not finalize job")
						}
					}
				}
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not drain")
				}
				key := repository.AccountConcurrencyKey(fx.account.ID)
				active, err := fx.concurrency.Current(context.Background(), key)
				if err != nil || active != 0 {
					t.Fatalf("worker exited with account lease still held: active=%d err=%v upstream_calls=%d", active, err, calls.Load())
				}
				release, acquired, err := fx.concurrency.Acquire(context.Background(), key, 1)
				if err != nil || !acquired {
					t.Fatalf("capacity unavailable after drain: acquired=%v err=%v", acquired, err)
				}
				release()
			})
		}
	}
}
