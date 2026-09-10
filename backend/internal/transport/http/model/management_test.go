package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestManagementPaginationAndPartialFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "management.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts, models := relational.NewAccountRepository(db), relational.NewModelRepository(db)
	var first uint64
	for index := range 23 {
		v, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: fmt.Sprintf("bind-%02d", index), SourceKey: fmt.Sprintf("bind-%02d", index), EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = v.ID
		}
	}
	route, err := models.Create(ctx, modeldomain.Route{PublicID: "original", Provider: account.ProviderBuild, UpstreamModel: "upstream", Capability: modeldomain.CapabilityResponses, Enabled: true}, []uint64{first})
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	NewHandler(modelapp.NewService(models, accounts, nil, nil)).Register(r.Group("/admin"))
	request := func(method, path, body string, status int) json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(method, "/admin"+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	for _, tc := range []struct {
		query                    string
		page, size, count, total int
	}{
		{"provider=grok_build&page=2&pageSize=20", 2, 20, 3, 23},
		{"provider=grok_build&search=bind-00", 1, 20, 1, 1},
		{fmt.Sprintf("provider=grok_build&search=%%23%d", first), 1, 20, 1, 1},
		{"provider=grok_web", 1, 20, 0, 0},
		{"provider=grok_build&page=-1&pageSize=999999", 1, 2000, 23, 23},
	} {
		var page struct {
			Items    []accountOptionResponse `json:"items"`
			Page     int                     `json:"page"`
			PageSize int                     `json:"pageSize"`
			Total    int                     `json:"total"`
		}
		if err := json.Unmarshal(request("GET", "/models/accounts?"+tc.query, "", 200), &page); err != nil {
			t.Fatal(err)
		}
		if page.Page != tc.page || page.PageSize != tc.size || page.Total != tc.total || len(page.Items) != tc.count {
			t.Fatalf("page %s = %+v", tc.query, page)
		}
	}
	request("GET", "/models/accounts?provider=unknown", "", 400)
	path := fmt.Sprintf("/models/%d", route.ID)
	request("PATCH", path, `{"enabled":false}`, 200)
	request("PATCH", path, `{"publicId":"renamed"}`, 200)
	request("PATCH", path, `{}`, 200)
	got, err := models.Get(ctx, route.ID)
	if err != nil || got.Enabled || got.PublicID != "Build/renamed" || len(got.BoundAccountIDs) != 1 {
		t.Fatalf("omitted fields: %+v err %v", got, err)
	}
	request("PATCH", path, `{"publicId":"","enabled":true}`, 400)
	request("PATCH", path, `{"accountIds":["0"]}`, 400)
	request("PATCH", path, `{"accountIds":[]}`, 200)
	got, err = models.Get(ctx, route.ID)
	if err != nil || got.Enabled || got.PublicID != "Build/renamed" || len(got.BoundAccountIDs) != 0 {
		t.Fatalf("explicit clear: %+v err %v", got, err)
	}
}
