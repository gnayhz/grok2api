package media

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

type inputHTTPObservedAssets struct {
	repository.MediaAssetRepository
	observed chan<- struct{}
	proceed  <-chan struct{}
}

func (r *inputHTTPObservedAssets) TotalMediaAssetBytes(ctx context.Context) (int64, error) {
	total, err := r.MediaAssetRepository.TotalMediaAssetBytes(ctx)
	if err != nil {
		return total, err
	}
	r.observed <- struct{}{}
	select {
	case <-r.proceed:
		return total, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestHTTPConcurrentInputUploadsShareCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []string{"image", "video"} {
			t.Run(dialect+"/"+kind, func(t *testing.T) {
				db, peer := uploadExpiryDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				objects, err := localmedia.NewLocalStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				var picture bytes.Buffer
				if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
					t.Fatal(err)
				}
				payload, mime := picture.Bytes(), "image/png"
				if kind == "video" {
					payload = append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 128)...)
					mime = "video/mp4"
				}
				cfg := mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: int64(len(payload)), CleanupThresholdPercent: 100}
				observed, proceed := make(chan struct{}, 2), make(chan struct{})
				databases := []*relational.Database{db, peer}
				services := make([]*mediaapp.Service, 0, 2)
				routers := make([]*gin.Engine, 0, 2)
				for _, database := range databases {
					source := &inputHTTPObservedAssets{relational.NewMediaAssetRepository(database), observed, proceed}
					service := mediaapp.NewServiceWithTickets(source, relational.NewMediaJobRepository(database), nil, objects, nil, cfg)
					services = append(services, service)
					router := gin.New()
					handler := NewHandler(service, nil)
					handler.RegisterAdmin(router.Group("/api/admin/v1"))
					handler.RegisterPublic(router.Group("/v1/media"))
					routers = append(routers, router)
				}
				results := make(chan *httptest.ResponseRecorder, 2)
				for _, router := range routers {
					var form bytes.Buffer
					writer := multipart.NewWriter(&form)
					header := textproto.MIMEHeader{}
					header.Set("Content-Disposition", `form-data; name="file"; filename="input"`)
					header.Set("Content-Type", mime)
					part, err := writer.CreatePart(header)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := part.Write(payload); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/upload", bytes.NewReader(form.Bytes())).WithContext(ctx)
					request.Header.Set("Content-Type", writer.FormDataContentType())
					go func(router *gin.Engine, request *http.Request) {
						response := httptest.NewRecorder()
						router.ServeHTTP(response, request)
						results <- response
					}(router, request)
				}
				for range 2 {
					select {
					case <-observed:
					case <-ctx.Done():
						close(proceed)
						t.Fatal(ctx.Err())
					}
				}
				close(proceed)
				created, refused, fileID := 0, 0, ""
				for range 2 {
					response := <-results
					switch response.Code {
					case http.StatusCreated:
						created++
						var body struct {
							Data struct {
								FileID string `json:"fileId"`
							} `json:"data"`
						}
						if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
							t.Fatal(err)
						}
						fileID = body.Data.FileID
					case http.StatusInsufficientStorage:
						refused++
						if !bytes.Contains(response.Body.Bytes(), []byte("mediaCapacityExceeded")) {
							t.Fatalf("wrong capacity failure: %s", response.Body.String())
						}
					default:
						t.Fatalf("input upload failed unexpectedly: %d %s", response.Code, response.Body.String())
					}
				}
				if created != 1 || refused != 1 || fileID == "" {
					t.Fatalf("HTTP admission counts=%d/%d id=%s", created, refused, fileID)
				}
				asset, body, err := services[1].OpenInputAsset(ctx, fileID)
				if err != nil {
					t.Fatal(err)
				}
				if err := body.Close(); err != nil {
					t.Fatal(err)
				}
				if asset.Kind != kind || asset.ExpiresAt == nil {
					t.Fatalf("input lost privacy/TTL: %+v", asset)
				}
				public := httptest.NewRecorder()
				routers[1].ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/v1/media/"+kind+"s/"+fileID, nil))
				if public.Code != http.StatusNotFound {
					t.Fatalf("input became public: %d", public.Code)
				}
				files, temps, err := objects.ListMediaObjectFiles(ctx)
				if err != nil || len(files) != 1 || len(temps) != 0 {
					t.Fatalf("refused HTTP input retained objects: %d/%d %v", len(files), len(temps), err)
				}
			})
		}
	}
}
