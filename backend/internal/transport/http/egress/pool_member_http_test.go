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
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func TestPoolMemberHTTPPreservesCurrentParentsAndPreference(t *testing.T) {
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
				db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "member-http.db"))
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
			pool, err := service.CreatePool(ctx, egressapp.PoolInput{Name: "members-http-pool"})
			if err != nil {
				t.Fatal(err)
			}
			defer service.DeletePool(ctx, pool.ID)
			var nodeIDs []uint64
			for _, name := range []string{"one", "two"} {
				proxy := "http://member-http.example:8080"
				node, err := service.Create(ctx, egressapp.Input{Name: name, Enabled: true, ProxyURL: &proxy})
				if err != nil {
					t.Fatal(err)
				}
				defer service.Delete(ctx, node.ID)
				nodeIDs = append(nodeIDs, node.ID)
			}
			poolPath := "/egress-pools/" + strconv.FormatUint(pool.ID, 10)
			ids := func(values ...uint64) map[string][]string {
				result := make([]string, 0, len(values))
				for _, id := range values {
					result = append(result, strconv.FormatUint(id, 10))
				}
				return map[string][]string{"nodeIds": result}
			}
			request(http.MethodPut, poolPath+"/members", ids(nodeIDs...), http.StatusOK)
			priorityPath := poolPath + "/members/" + strconv.FormatUint(nodeIDs[0], 10) + "/priority"
			request(http.MethodPut, priorityPath, map[string]any{"priority": 5}, http.StatusOK)
			request(http.MethodPut, poolPath+"/members", ids(nodeIDs[0], nodeIDs[1], nodeIDs[0]), http.StatusOK)
			preferred, err := repo.EgressPoolPreferredNodes(ctx)
			if err != nil || preferred[pool.ID] != nodeIDs[0] {
				t.Fatalf("replacement lost preference: %v %v", preferred, err)
			}
			request(http.MethodPut, poolPath+"/members", ids(nodeIDs[0], 999999999), http.StatusBadRequest)
			request(http.MethodPut, priorityPath, map[string]any{"priority": -1}, http.StatusBadRequest)
			members, err := repo.EgressPoolMembers(ctx)
			if err != nil || len(members[pool.ID]) != 2 {
				t.Fatalf("bad write lost members: %v %v", members, err)
			}
			request(http.MethodPut, poolPath+"/members", ids(), http.StatusOK)
			request(http.MethodPut, priorityPath, map[string]any{"priority": 1}, http.StatusBadRequest)
			request(http.MethodPut, poolPath+"/members", ids(nodeIDs...), http.StatusOK)
			request(http.MethodDelete, "/egress-nodes/"+strconv.FormatUint(nodeIDs[0], 10), nil, http.StatusOK)
			request(http.MethodPut, poolPath+"/members", ids(nodeIDs...), http.StatusBadRequest)
			members, err = repo.EgressPoolMembers(ctx)
			if err != nil || len(members[pool.ID]) != 1 || members[pool.ID][0] != nodeIDs[1] {
				t.Fatalf("deleted node still member: %v %v", members, err)
			}
			request(http.MethodDelete, poolPath, nil, http.StatusOK)
			request(http.MethodPut, poolPath+"/members", ids(nodeIDs[1]), http.StatusNotFound)

		})
	}
}
