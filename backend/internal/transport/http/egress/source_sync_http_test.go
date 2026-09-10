package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func TestSourceSyncHTTPRetiresOldDownloadAndPreservesPublicResult(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			var db *relational.Database
			var err error
			if dialect == "postgres" {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated TEST_POSTGRES_DSN")
				}
				db, err = relational.OpenPostgres(ctx, dsn, 4, 2)
			} else {
				db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "source-sync-http.db"))
			}
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			repo := relational.NewEgressRepository(db)
			service := egressapp.NewService(repo, cipher)
			defer service.Close(ctx)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			NewHandler(service).Register(router.Group("/admin"))
			server := httptest.NewServer(router)
			defer server.Close()
			type httpResult struct {
				status int
				body   []byte
				err    error
			}
			call := func(method, path string, input any) httpResult {
				data, err := json.Marshal(input)
				if err != nil {
					return httpResult{err: err}
				}
				req, err := http.NewRequestWithContext(ctx, method, server.URL+"/admin"+path, bytes.NewReader(data))
				if err != nil {
					return httpResult{err: err}
				}
				req.Header.Set("Content-Type", "application/json")
				res, err := server.Client().Do(req)
				if err != nil {
					return httpResult{err: err}
				}
				defer res.Body.Close()
				data, err = io.ReadAll(res.Body)
				return httpResult{status: res.StatusCode, body: data, err: err}
			}
			check := func(result httpResult, status int) json.RawMessage {
				t.Helper()
				if result.err != nil || result.status != status {
					t.Fatalf("HTTP status=%d body=%s err=%v", result.status, result.body, result.err)
				}
				var envelope struct {
					Data  json.RawMessage `json:"data"`
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(result.body, &envelope); err != nil {
					t.Fatal(err)
				}
				if status == 502 && envelope.Error.Code != "egressSubscriptionSyncFailed" {
					t.Fatalf("public error changed: %s", result.body)
				}
				if bytes.Contains(result.body, []byte("syncRevision")) || bytes.Contains(result.body, []byte("secret-feed-token")) {
					t.Fatal("source secret or internal revision exposed")
				}
				return envelope.Data
			}
			for _, oldFailure := range []bool{false, true} {
				ready, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/old" {
						close(ready)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
						if oldFailure {
							http.Error(w, "old upstream failed", 503)
							return
						}
					}
					_, _ = io.WriteString(w, "http://source-http.example:8080\n")
				}))
				defer feed.Close()
				name := "source-http-" + strconv.FormatInt(time.Now().UnixNano(), 10)
				data := check(call("POST", "/egress-sources", map[string]any{"name": name, "enabled": true, "url": "http://1.1.1.1/old?secret-feed-token", "proxyURL": feed.URL}), 201)
				var source struct {
					ID uint64 `json:"id,string"`
				}
				if err := json.Unmarshal(data, &source); err != nil {
					t.Fatal(err)
				}
				path := "/egress-sources/" + strconv.FormatUint(source.ID, 10)
				done := make(chan httpResult, 1)
				go func() { done <- call("POST", path+"/sync", nil) }()
				select {
				case <-ready:
				case <-time.After(10 * time.Second):
					t.Fatal("source download did not start")
				}
				check(call("PUT", path, map[string]any{"name": name, "enabled": true, "url": "http://1.1.1.1/new?secret-feed-token"}), 200)
				before, err := repo.GetEgressSource(ctx, source.ID)
				if err != nil {
					t.Fatal(err)
				}
				unblock()
				check(<-done, 502)
				after, err := repo.GetEgressSource(ctx, source.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("retired HTTP sync changed metadata")
				}
				nodes, err := repo.ListEgressNodesFromSource(ctx, source.ID)
				if err != nil || len(nodes) != 0 {
					t.Fatalf("retired HTTP sync imported nodes: %v %v", nodes, err)
				}
				check(call("POST", path+"/sync", nil), 200)
				final, err := repo.GetEgressSource(ctx, source.ID)
				if err != nil || final.LastSyncImported != 1 || final.LastSyncError != "" {
					t.Fatalf("current sync failed: %+v %v", final, err)
				}
				check(call("GET", "/egress-sources", nil), 200)
				check(call("DELETE", path, nil), 200)
				nodes, err = repo.ListEgressNodesFromSource(ctx, source.ID)
				if err != nil || len(nodes) != 0 {
					t.Fatalf("deleted source association retained: %v %v", nodes, err)
				}
				feed.Close()
			}
		})
	}
}
