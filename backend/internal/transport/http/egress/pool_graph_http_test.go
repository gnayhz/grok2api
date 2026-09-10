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

func TestPoolGraphHTTPPreservesValidConfiguration(t *testing.T) {
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
				db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "pool-http.db"))
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
			request := func(method, path string, body poolRequest, status int) json.RawMessage {
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
			create := func(name, fallback string) poolResponse {
				t.Helper()
				body := poolRequest{Name: name, Strategy: "random", FallbackMode: "none"}
				if fallback != "" {
					body.FallbackMode, body.FallbackPoolID = "pool", fallback
				}
				var pool poolResponse
				if err := json.Unmarshal(request(http.MethodPost, "/egress-pools", body, http.StatusCreated), &pool); err != nil {
					t.Fatal(err)
				}
				return pool
			}
			first := create("graph-http-first", "")
			firstID := strconv.FormatUint(first.ID, 10)
			second := create("graph-http-second", firstID)
			secondID := strconv.FormatUint(second.ID, 10)
			defer service.DeletePool(ctx, first.ID)
			defer service.DeletePool(ctx, second.ID)
			for _, target := range []string{secondID, firstID, "999999999"} {
				request(http.MethodPut, "/egress-pools/"+firstID, poolRequest{Name: first.Name, FallbackMode: "pool", FallbackPoolID: target}, http.StatusBadRequest)
			}
			request(http.MethodPost, "/egress-pools", poolRequest{Name: "graph-http-missing", FallbackMode: "pool", FallbackPoolID: "999999999"}, http.StatusBadRequest)
			request(http.MethodDelete, "/egress-pools/"+secondID, poolRequest{}, http.StatusOK)
			request(http.MethodPut, "/egress-pools/"+firstID, poolRequest{Name: first.Name, FallbackMode: "pool", FallbackPoolID: secondID}, http.StatusBadRequest)
			request(http.MethodPut, "/egress-pools/"+firstID, poolRequest{Name: first.Name, Strategy: "sticky", FallbackMode: "direct"}, http.StatusOK)
			stored, err := repo.GetEgressPool(ctx, first.ID)
			if err != nil || stored.Strategy != domain.PoolStrategySticky || stored.FallbackMode != domain.PoolFallbackDirect || stored.FallbackPoolID != 0 {
				t.Fatalf("valid retry did not persist: %+v %v", stored, err)
			}
		})
	}
}
