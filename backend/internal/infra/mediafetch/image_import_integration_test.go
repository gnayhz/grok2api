package mediafetch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
	mediahttp "github.com/chenyme/grok2api/backend/internal/transport/http/media"
	"github.com/gin-gonic/gin"
)

func imageImportDatabase(t *testing.T, dialect string) *relational.Database {
	t.Helper()
	ctx := context.Background()
	var db *relational.Database
	var err error
	if dialect == "sqlite" {
		db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "import.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("isolated TEST_POSTGRES_DSN required")
		}
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("g49_import_%d", time.Now().UnixNano())
		if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			_ = admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer admin.Close()
			if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		db, err = relational.OpenPostgres(ctx, parsed.String(), 4, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

type invalidImageRegistration struct {
	repository.MediaAssetRepository
}

func (r invalidImageRegistration) CreateMediaInputAsset(ctx context.Context, asset mediadomain.Asset, limit int64) error {
	asset.SHA256 = "invalid" // The actual SQL shape constraint rejects registration.
	return r.MediaAssetRepository.CreateMediaInputAsset(ctx, asset, limit)
}

func TestImageImportHTTPUsesApplicationPolicyAndActualStorage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, scenario := range []string{"success", "invalid_url", "blocked_redirect", "fetch_failure", "oversize", "invalid_image", "capacity", "sql_failure"} {
			t.Run(dialect+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				picture := imageNetworkBytes(t)
				network := newImageNetworkFixture(t, func(w http.ResponseWriter, r *http.Request) {
					switch scenario {
					case "blocked_redirect":
						http.Redirect(w, r, "http://127.0.0.1/private", http.StatusFound)
					case "fetch_failure":
						http.Error(w, "service down", http.StatusServiceUnavailable)
					case "oversize":
						w.Header().Set("Content-Length", fmt.Sprint(mediadomain.MaxInputAssetBytes+1))
						_, _ = w.Write(picture)
					case "invalid_image":
						_, _ = io.WriteString(w, "not an image")
					default:
						w.Header().Set("Content-Type", "image/png")
						_, _ = w.Write(picture)
					}
				})
				db := imageImportDatabase(t, dialect)
				objects, err := localmedia.NewLocalStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				assets := relational.NewMediaAssetRepository(db)
				var assetPort repository.MediaAssetRepository = assets
				if scenario == "sql_failure" {
					assetPort = invalidImageRegistration{assets}
				}
				cfg := mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 100}
				if scenario == "capacity" {
					cfg.MaxTotalBytes = int64(len(picture) - 1)
				}
				service := mediaapp.NewServiceWithTickets(assetPort, relational.NewMediaJobRepository(db), nil, objects, nil, cfg)
				importer := mediaapp.NewImageInputImporter(service, network.source)
				handler := mediahttp.NewHandler(service, importer)
				router := gin.New()
				handler.RegisterPublic(router.Group("/v1/media"))
				handler.RegisterAdmin(router.Group("/api/admin/v1"))
				target := "https://example.com/image"
				if scenario == "invalid_url" {
					target = "https://user:password@example.com/private"
				}
				body, _ := json.Marshal(map[string]string{"url": target})
				request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/import", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				statuses := map[string]int{"success": 201, "invalid_url": 400, "blocked_redirect": 400, "fetch_failure": 502, "oversize": 413, "invalid_image": 400, "capacity": 507, "sql_failure": 500}
				codes := map[string]string{"invalid_url": "invalidImageURL", "blocked_redirect": "imageURLBlocked", "fetch_failure": "imageFetchFailed", "oversize": "imageTooLarge", "invalid_image": "invalidImage", "capacity": "mediaCapacityExceeded", "sql_failure": "mediaSaveImageFailed"}
				if response.Code != statuses[scenario] {
					t.Fatalf("status=%d want=%d response=%s", response.Code, statuses[scenario], response.Body.String())
				}
				var envelope struct {
					Data struct {
						FileID    string `json:"fileId"`
						Kind      string `json:"kind"`
						MIMEType  string `json:"mimeType"`
						SizeBytes int64  `json:"sizeBytes"`
						ExpiresAt string `json:"expiresAt"`
					} `json:"data"`
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				files, temps, err := objects.ListMediaObjectFiles(ctx)
				if err != nil {
					t.Fatal(err)
				}
				total, err := assets.TotalMediaAssetBytes(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "success" {
					if !mediadomain.IsInputAssetID(envelope.Data.FileID) || envelope.Data.Kind != "image" || envelope.Data.MIMEType != "image/png" || envelope.Data.SizeBytes != int64(len(picture)) || envelope.Data.ExpiresAt == "" || len(files) != 1 || len(temps) != 0 || total != int64(len(picture)) {
						t.Fatalf("private asset envelope/resource mismatch: %s files=%d/%d total=%d", response.Body.String(), len(files), len(temps), total)
					}
					asset, body, err := service.OpenInputAsset(ctx, envelope.Data.FileID)
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(body)
					_ = body.Close()
					if err != nil || !bytes.Equal(data, picture) || asset.ExpiresAt == nil || time.Until(*asset.ExpiresAt) < 23*time.Hour {
						t.Fatalf("imported bytes/TTL changed: %v", err)
					}
					public := httptest.NewRecorder()
					router.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/v1/media/images/"+asset.ID, nil))
					if public.Code != 404 {
						t.Fatal("imported input became public")
					}
					if _, count, err := service.AdminListImages(ctx, 1, 20, ""); err != nil || count != 0 {
						t.Fatalf("input appeared in gallery: count=%d err=%v", count, err)
					}
				} else {
					if envelope.Error.Code != codes[scenario] {
						t.Fatalf("error code=%q want=%q body=%s", envelope.Error.Code, codes[scenario], response.Body.String())
					}
					if total != 0 || len(files)+len(temps) != 0 {
						t.Fatalf("failed import retained SQL/object resources: total=%d files=%d/%d", total, len(files), len(temps))
					}
				}
				if scenario == "invalid_url" && network.calls.Load() != 0 {
					t.Fatal("invalid URL reached source")
				}
				network.assertClosed(t)
			})
		}
	}
}

func TestImageImportHTTPCancellationReleasesNetworkAndUploadSlots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	picture := imageNetworkBytes(t)
	started := make(chan struct{}, 4)
	var blocked atomic.Bool
	blocked.Store(true)
	network := newImageNetworkFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if blocked.Load() {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(picture)
	})
	db := imageImportDatabase(t, "sqlite")
	objects, err := localmedia.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	assets := relational.NewMediaAssetRepository(db)
	service := mediaapp.NewServiceWithTickets(assets, nil, nil, objects, nil, mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80})
	router := gin.New()
	mediahttp.NewHandler(service, mediaapp.NewImageInputImporter(service, network.source)).RegisterAdmin(router.Group("/api/admin/v1"))
	request := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/import", strings.NewReader(`{"url":"https://example.com/image"}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	importsCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan *httptest.ResponseRecorder, 4)
	for range 4 {
		go func() { done <- request(importsCtx) }()
	}
	for range 4 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("imports did not reach four live TLS bodies")
		}
	}
	fifth := request(ctx)
	if fifth.Code != http.StatusServiceUnavailable || !bytes.Contains(fifth.Body.Bytes(), []byte(`"mediaIngestBusy"`)) || network.calls.Load() != 4 {
		t.Fatalf("ingest bound failed: code=%d calls=%d", fifth.Code, network.calls.Load())
	}
	stop()
	for range 4 {
		select {
		case out := <-done:
			if out.Code == http.StatusCreated {
				t.Fatal("canceled input accepted")
			}
		case <-ctx.Done():
			t.Fatal("canceled imports did not finish")
		}
	}
	network.assertClosed(t)
	total, err := assets.TotalMediaAssetBytes(ctx)
	if err != nil || total != 0 {
		t.Fatalf("canceled inputs persisted: %d %v", total, err)
	}
	files, temps, err := objects.ListMediaObjectFiles(ctx)
	if err != nil || len(files)+len(temps) != 0 {
		t.Fatalf("canceled imports leaked files: %d/%d %v", len(files), len(temps), err)
	}
	blocked.Store(false)
	if out := request(ctx); out.Code != http.StatusCreated {
		t.Fatalf("import slot not released: %d %s", out.Code, out.Body.String())
	}
	network.assertClosed(t)
}
