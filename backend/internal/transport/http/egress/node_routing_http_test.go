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
	"strconv"
	"testing"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func TestFixedRoutingHTTPPreservesNodeConfigurationAndNotFound(t *testing.T) {
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
				db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "fixed-routing-http.db"))
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
			request := func(method, path string, body any, status int) json.RawMessage {
				t.Helper()
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequestWithContext(ctx, method, server.URL+"/admin"+path, bytes.NewReader(encoded))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				response, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				payload, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != status {
					t.Fatalf("%s %s: status=%d payload=%s", method, path, response.StatusCode, payload)
				}
				var result struct {
					Data  json.RawMessage       `json:"data"`
					Error struct{ Code string } `json:"error"`
				}
				if err := json.Unmarshal(payload, &result); err != nil {
					t.Fatal(err)
				}
				if status == http.StatusBadRequest && result.Error.Code != "invalidEgressNode" {
					t.Fatalf("graph error lost public code: %s", payload)
				}
				return result.Data
			}
			proxy := "http://fixed-http.example:8080"
			node, err := service.Create(ctx, egressapp.Input{Name: "fixed-http-node", Enabled: true, ProxyURL: &proxy})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Delete(ctx, node.ID)
			nodePath := "/egress-nodes/" + strconv.FormatUint(node.ID, 10)
			target := egressapp.RoutingTargetInput{Mode: domain.RoutingTargetNode, NodeID: node.ID}
			if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{ProbeIntervalSeconds: 900, DefaultTarget: &target}); err != nil {
				t.Fatal(err)
			}
			for _, body := range []map[string]any{
				{"name": node.Name, "enabled": false},
				{"name": node.Name, "enabled": true, "clearProxyURL": true},
				{"name": node.Name, "enabled": true, "proxyURL": "http://user-{account}:password@fixed-http.example:8080"},
			} {
				request(http.MethodPut, nodePath, body, http.StatusBadRequest)
			}
			request(http.MethodPut, nodePath, map[string]any{"name": "renamed-fixed-node", "enabled": true}, http.StatusOK)
			stored, err := repo.GetEgressNode(ctx, node.ID)
			if err != nil || !stored.Enabled || stored.Name != "renamed-fixed-node" || stored.EncryptedProxyURL == "" {
				t.Fatalf("fixed node damaged: %+v %v", stored, err)
			}
			request(http.MethodDelete, nodePath, nil, http.StatusOK)
			request(http.MethodPut, nodePath, map[string]any{"name": "deleted", "enabled": true}, http.StatusNotFound)
			pool, err := service.CreatePool(ctx, egressapp.PoolInput{Name: "delete-http-pool"})
			if err != nil {
				t.Fatal(err)
			}
			poolPath := "/egress-pools/" + strconv.FormatUint(pool.ID, 10)
			request(http.MethodDelete, poolPath, nil, http.StatusOK)
			request(http.MethodDelete, poolPath, nil, http.StatusNotFound)

		})
	}
}
