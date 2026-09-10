package inference

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type videoResourceJobFault struct {
	repository.MediaJobRepository
	fail  atomic.Bool
	cause error
}

func (s *videoResourceJobFault) GetMediaJob(ctx context.Context, id string, key uint64) (media.Job, error) {
	if s.fail.Load() {
		return media.Job{}, s.cause
	}
	return s.MediaJobRepository.GetMediaJob(ctx, id, key)
}

type videoResourceAssetFault struct {
	repository.MediaAssetRepository
	fail  atomic.Bool
	cause error
}

func (s *videoResourceAssetFault) GetMediaAsset(ctx context.Context, id string) (media.Asset, error) {
	if s.fail.Load() {
		return media.Asset{}, s.cause
	}
	return s.MediaAssetRepository.GetMediaAsset(ctx, id)
}

type videoResourceObjectFault struct {
	repository.MediaObjectStorage
	fail  atomic.Bool
	cause error
}

func (s *videoResourceObjectFault) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.fail.Load() {
		return nil, s.cause
	}
	return s.MediaObjectStorage.Open(ctx, key)
}

func TestHTTPVideoResourceFailureClassification(t *testing.T) {
	if !imageAssetTLSChild(t) {
		return
	}
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db := compactionDatabase(t, dialect)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			accounts, audits, keys := relational.NewAccountRepository(db), relational.NewAuditRepository(db), relational.NewClientKeyRepository(db)
			encrypted, err := cipher.Encrypt("synthetic-video-token")
			if err != nil {
				t.Fatal(err)
			}
			credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, SourceKey: "video-resource", Name: "video-resource", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			models := relational.NewModelRepository(db)
			if err := testsupport.Discover(ctx, models, account.ProviderBuild, []string{"grok-imagine-video-1.5"}); err != nil {
				t.Fatal(err)
			}
			if err := testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{"grok-imagine-video-1.5"}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			route, err := models.GetByProviderUpstream(ctx, account.ProviderBuild, "grok-imagine-video-1.5")
			if err != nil {
				t.Fatal(err)
			}
			capacity, sticky := memory.NewConcurrencyLimiter(), memory.NewStickyStore()
			clients := clientkeyapp.NewService("video-resources", keys, memory.NewRateLimiter(), capacity, 1000, 8, cipher)
			t.Cleanup(func() { closeClientKeyService(t, clients) })
			owner, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "owner", Enabled: true, RPMLimit: 1000, MaxConcurrent: 8})
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "foreign", Enabled: true, RPMLimit: 1000, MaxConcurrent: 8})
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("private database path and object endpoint")
			jobs := &videoResourceJobFault{MediaJobRepository: relational.NewMediaJobRepository(db), cause: cause}
			assets := &videoResourceAssetFault{MediaAssetRepository: relational.NewMediaAssetRepository(db), cause: cause}
			disk, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
			if err != nil {
				t.Fatal(err)
			}
			objects := &videoResourceObjectFault{MediaObjectStorage: disk, cause: cause}
			local := mediaapp.NewService(assets, jobs, objects, nil, mediaapp.Config{MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute})
			payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{3}, 64)...)
			asset, err := local.SaveVideo(ctx, "", "video/mp4", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			job := media.Job{ID: "video_resource", RequestID: "resource", ClientKeyID: owner.Key.ID, AccountID: credential.ID, Provider: string(account.ProviderBuild), Model: "grok-imagine-video-1.5", ModelRouteID: route.ID, UpstreamModel: "grok-imagine-video-1.5", Status: media.StatusCompleted, ResultAssetID: asset.ID, UpstreamURL: "https://assets.grok.com/video.mp4", CreatedAt: now, UpdatedAt: now, CompletedAt: &now, UsageRecordedAt: &now}
			if err = jobs.CreateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			var download, primaryCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				http.Error(w, "unexpected generation", 500)
			}))
			t.Cleanup(primary.Close)
			proxyURL := imageAssetProxy(t, primary.URL, func(w http.ResponseWriter, r *http.Request) {
				download.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/video.mp4" {
					t.Error("unexpected asset request")
				}
				for _, name := range []string{"Authorization", "Cookie", "Token-Auth", "X-Api-Key"} {
					if r.Header.Get(name) != "" {
						t.Error("asset download forwarded private identity header")
					}
				}
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = io.WriteString(w, "remote-video")
			})
			repo := relational.NewEgressRepository(db)
			encryptedProxy, err := cipher.Encrypt(proxyURL)
			if err != nil {
				t.Fatal(err)
			}
			node, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: "video-test", Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = repo.SaveEgressOperationsConfig(ctx, egressdomain.OperationsConfig{DefaultTarget: egressdomain.RoutingTarget{Mode: egressdomain.RoutingTargetNode, NodeID: node.ID}}, func(egressdomain.Node) error {
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			network := infraegress.NewManager(repo, cipher)
			t.Cleanup(func() { _ = network.Close(context.Background()) })
			build := cli.NewAdapter(cli.Config{BaseURL: primary.URL + "/v1"}, cipher)
			build.SetEgress(network)
			registry := provider.NewRegistry(build)
			maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
			selector := gateway.NewSelector(accounts, capacity, sticky, registry, time.Hour, time.Second, time.Minute)
			buildRouter := func() *gin.Engine {
				service := gateway.NewService(relational.NewModelRepository(db), audits, maintenance, clients, registry, selector, relational.NewResponseRepository(db), 2)
				service.ConfigureMedia(jobs, 1)
				service.ConfigureMediaAssets(local)
				r := gin.New()
				r.Use(middleware.RequestID(), middleware.ClientAuth(clients))
				NewHandler(service, nil, 1<<20).Register(r.Group("/v1"))
				return r
			}
			var router atomic.Pointer[gin.Engine]
			router.Store(buildRouter())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { router.Load().ServeHTTP(w, r) }))
			t.Cleanup(server.Close)
			send := func(path, secret string, status int, match string) {
				t.Helper()
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+secret)
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != status || match != "" && !strings.Contains(string(body), match) {
					t.Errorf("%s status=%d want=%d body=%s", path, response.StatusCode, status, body)
				}
				if strings.Contains(string(body), "private") {
					t.Errorf("private cause exposed: %s", body)
				}
			}
			for _, suffix := range []string{"", "/content"} {
				send("/v1/videos/"+job.ID+suffix, foreign.Secret, 404, "response_not_found")
				send("/v1/videos/absent"+suffix, owner.Secret, 404, "response_not_found")
			}
			send("/v1/videos/"+job.ID, owner.Secret, 200, asset.ID)
			send("/v1/videos/"+job.ID+"/content", owner.Secret, 200, "")
			for _, fault := range []struct {
				name string
				flag *atomic.Bool
			}{{"job", &jobs.fail}, {"metadata", &assets.fail}, {"object", &objects.fail}} {
				t.Run(fault.name, func(t *testing.T) {
					before := download.Load()
					fault.flag.Store(true)
					send("/v1/videos/"+job.ID, owner.Secret, 503, "video_state_unavailable")
					send("/v1/videos/"+job.ID+"/content", owner.Secret, 503, "video_state_unavailable")
					fault.flag.Store(false)
					if download.Load() != before {
						t.Error("local storage error dispatched upstream fallback")
					}
					router.Store(buildRouter())
					send("/v1/videos/"+job.ID, owner.Secret, 200, asset.ID)
					send("/v1/videos/"+job.ID+"/content", owner.Secret, 200, "")
					stored, err := jobs.MediaJobRepository.GetMediaJob(ctx, job.ID, owner.Key.ID)
					if err != nil || stored.ResultAssetID != asset.ID || stored.Status != media.StatusCompleted {
						t.Fatalf("read failure changed durable job: %v %+v", err, stored)
					}
				})
			}
			if err = objects.Delete(ctx, asset.StorageKey); err != nil {
				t.Fatal(err)
			}
			send("/v1/videos/"+job.ID, owner.Secret, 200, "/content")
			before := download.Load()
			send("/v1/videos/"+job.ID+"/content", owner.Secret, 200, "remote-video")
			if download.Load() != before+1 {
				t.Fatal("confirmed missing object did not keep fallback")
			}
			if primaryCalls.Load() != 0 {
				t.Fatal("resource access generated a video")
			}
		})
	}
}
