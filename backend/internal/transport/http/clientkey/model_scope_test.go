package clientkey

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func TestModelScopeHTTPCommandsPreserveExplicitRestrictions(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	models, keys := relational.NewModelRepository(db), relational.NewClientKeyRepository(db)
	route, err := models.Create(ctx, model.Route{PublicID: "allowed", Provider: account.ProviderBuild, UpstreamModel: "grok-4.3", Capability: model.CapabilityResponses, Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	service := clientkeyapp.NewService("test", keys, nil, nil, 0, 0, cipher)
	defer service.Close(ctx)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(service).Register(router.Group("/api"))
	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		router.ServeHTTP(result, request)
		if result.Code != want {
			t.Fatalf("%s %s: %d %s want=%d", method, path, result.Code, result.Body.String(), want)
		}
		return result
	}
	response := call("POST", "/api/client-keys", fmt.Sprintf(`{"name":"restricted","allowedModelIds":["%d","%d"]}`, route.ID, route.ID), 201)
	var created struct {
		Data struct {
			Key    keyResponse `json:"key"`
			Secret string      `json:"secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.Key.ModelScope != clientkeydomain.ModelScopeRestricted || len(created.Data.Key.AllowedModelIDs) != 1 {
		t.Fatalf("legacy list creation: %s", response.Body.String())
	}
	id := created.Data.Key.ID
	path := fmt.Sprintf("/api/client-keys/%d", id)
	if err := models.Delete(ctx, route.ID); err != nil {
		t.Fatal(err)
	}
	response = call("PATCH", path, `{"name":"renamed"}`, 200)
	var updated struct {
		Data keyResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Data.ModelScope != clientkeydomain.ModelScopeRestricted || len(updated.Data.AllowedModelIDs) != 0 {
		t.Fatalf("name patch lost empty restriction: %s", response.Body.String())
	}
	key, release, err := service.Authenticate(ctx, created.Data.Secret)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if key.AllowsModel(999) {
		t.Fatal("HTTP-renamed empty restriction authorizes models")
	}
	for _, body := range []string{`{"modelScope":""}`, `{"modelScope":"invalid"}`, `{"modelScope":"all","allowedModelIds":["999"]}`, `{"allowedModelIds":["0"]}`, `{"name":"must rollback","allowedModelIds":["999"]}`} {
		call("PATCH", path, body, 400)
		value, err := keys.Get(ctx, id)
		if err != nil || value.Name != "renamed" || value.ModelScope != clientkeydomain.ModelScopeRestricted {
			t.Fatalf("invalid HTTP patch changed key: %+v %v", value, err)
		}
	}
	call("PATCH", path, `{"modelScope":"restricted","allowedModelIds":[]}`, 200)
	call("PATCH", path, `{"allowedModelIds":[]}`, 200)
	value, err := keys.Get(ctx, id)
	if err != nil || value.ModelScope != clientkeydomain.ModelScopeAll || !value.AllowsModel(999) {
		t.Fatalf("legacy clear didn't authorize all: %+v %v", value, err)
	}
	call("PATCH", path, `{"modelScope":"restricted"}`, 200)
	value, err = keys.Get(ctx, id)
	if err != nil || value.ModelScope != clientkeydomain.ModelScopeRestricted || value.AllowsModel(999) {
		t.Fatalf("empty explicit restriction lost: %+v %v", value, err)
	}
	call("POST", "/api/client-keys", `{"name":"empty restricted","modelScope":"restricted"}`, 201)
	call("POST", "/api/client-keys", `{"name":"all"}`, 201)
	for _, body := range []string{`{"name":"bad","modelScope":""}`, `{"name":"bad","modelScope":"unknown"}`, `{"name":"bad","allowedModelIds":["999"]}`} {
		call("POST", "/api/client-keys", body, 400)
	}
	for _, scope := range []string{"all", "restricted"} {
		result := call("GET", "/api/client-keys?modelScope="+scope, "", 200)
		var page struct {
			Data struct {
				Items []keyResponse `json:"items"`
				Total int           `json:"total"`
			} `json:"data"`
		}
		if err := json.Unmarshal(result.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		want := 2
		if scope == "all" {
			want = 1
		}
		if page.Data.Total != want || len(page.Data.Items) != want {
			t.Fatalf("scope filter %s: %s", scope, result.Body.String())
		}
		for _, key := range page.Data.Items {
			if string(key.ModelScope) != scope {
				t.Fatalf("wrong scope in %s", result.Body.String())
			}
		}
		if scope == "restricted" {
			if path := os.Getenv("TEST_CLIENT_KEY_SCOPE_FIXTURE"); path != "" {
				if err := os.WriteFile(path, result.Body.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
