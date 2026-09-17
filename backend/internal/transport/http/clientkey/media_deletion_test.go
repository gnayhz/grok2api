package clientkey

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	mediahttp "github.com/chenyme/grok2api/backend/internal/transport/http/media"
	"github.com/gin-gonic/gin"
)

func TestKeyAndMediaHTTPDeletionPreservePendingCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "deletion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repo, jobs := relational.NewClientKeyRepository(db), relational.NewMediaJobRepository(db)
	keys := clientkeyapp.NewService("http-deletion", repo, nil, nil, 0, 0, cipher, security.RandomTokenSource{})
	defer keys.Close(ctx)
	router := gin.New()
	NewHandler(keys).Register(router.Group("/api"))
	mediahttp.NewHandler(mediaapp.NewServiceWithTickets(relational.NewMediaAssetRepository(db), jobs, nil, nil, nil, mediaapp.Config{}), nil).RegisterAdmin(router.Group("/api"))
	call := func(path, body string, want int, code string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != want || code != "" && !strings.Contains(recorder.Body.String(), code) {
			t.Fatalf("DELETE %s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	for _, entry := range []string{"single", "batch", "media"} {
		for _, stage := range []string{"queued", "in_progress", "quota", "usage", "complete"} {
			t.Run(entry+"/"+stage, func(t *testing.T) {
				created, err := keys.Create(ctx, clientkeyapp.CreateInput{Name: entry + "-" + stage, Enabled: true, RPMUnlimited: true, ConcurrencyUnlimited: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, release, err := keys.Authenticate(ctx, created.Secret); err != nil {
					t.Fatal(err)
				} else {
					release()
				}
				now := time.Now().UTC()
				job := media.Job{ID: fmt.Sprintf("video_http_delete_%d", created.Key.ID), RequestID: "request_http_delete", ClientKeyID: created.Key.ID, ClientKeyName: created.Key.Name, Provider: "grok_web", Model: "Web/grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video", Prompt: "synthetic", Seconds: 3, Quality: "720p", Status: media.StatusCompleted, CreatedAt: now, UpdatedAt: now, Quota: media.JobQuota{RecordedAt: &now}, UsageRecordedAt: &now}
				if stage == "queued" || stage == "in_progress" {
					job.Status = media.Status(stage)
				} else if stage == "quota" {
					job.Quota.RecordedAt = nil
				} else if stage == "usage" {
					job.UsageRecordedAt = nil
				}
				if err := jobs.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				path, body := fmt.Sprintf("/api/client-keys/%d", created.Key.ID), ""
				if entry == "batch" {
					path = "/api/client-keys"
					payload, _ := json.Marshal(map[string][]string{"ids": {fmt.Sprint(created.Key.ID)}})
					body = string(payload)
				} else if entry == "media" {
					path = "/api/media/videos"
					payload, _ := json.Marshal(map[string][]string{"ids": {job.ID}})
					body = string(payload)
				}
				if stage != "complete" {
					status, code := http.StatusConflict, "clientKeyConflict"
					if entry == "media" {
						status, code = http.StatusBadRequest, "invalidVideoSelection"
					}
					call(path, body, status, code)
					if _, release, err := keys.Authenticate(ctx, created.Secret); err != nil {
						t.Fatalf("rejected deletion revoked valid key: %v", err)
					} else {
						release()
					}
					if stage == "queued" || stage == "in_progress" {
						job.Status = media.StatusCompleted
						if err := jobs.UpdateMediaJob(ctx, job); err != nil {
							t.Fatal(err)
						}
					}
					if err := jobs.MarkMediaJobQuotaRecorded(ctx, job, now); err != nil {
						t.Fatal(err)
					}
					if err := jobs.MarkMediaJobUsageRecorded(ctx, job.ID, now); err != nil {
						t.Fatal(err)
					}
				}
				call(path, body, http.StatusOK, "deleted")
				if _, err := jobs.GetMediaJob(ctx, job.ID, created.Key.ID); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("HTTP success retained job: %v", err)
				}
				if entry != "media" {
					if _, release, err := keys.Authenticate(ctx, created.Secret); !errors.Is(err, clientkeyapp.ErrInvalidKey) {
						if release != nil {
							release()
						}
						t.Fatalf("successful delete left cached authorization: %v", err)
					}
				}
				if entry == "single" {
					call(path, "", http.StatusNotFound, "clientKeyNotFound")
				} else {
					call(path, body, http.StatusOK, `"deleted":0`)
				}
			})
		}
	}
}
