package model

import (
	"context"
	"fmt"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagementRejectsInvalidCapabilityAndPreservesLegacyRoute(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "capability.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	models := relational.NewModelRepository(db)
	registry := provider.NewRegistry(cli.NewAdapter(cli.Config{}, nil), web.NewAdapter(web.Config{}, nil, nil, nil, nil), console.NewAdapter(console.Config{}, nil, nil, nil))
	svc := modelapp.NewService(models, relational.NewAccountRepository(db), nil, registry)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(svc).Register(r.Group("/admin"))
	request := func(method, path, body string, want int) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/admin"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	for _, tc := range []struct{ p, up, cap string }{{"grok_web", "grok-chat-fast", "video"}, {"grok_console", "grok-imagine-image", "responses"}, {"grok_build", "future-text", "video"}, {"grok_build", modeldomain.BuildVideoModel, "responses"}} {
		body := fmt.Sprintf(`{"publicId":"custom","provider":%q,"upstreamModel":%q,"capability":%q,"enabled":true}`, tc.p, tc.up, tc.cap)
		got := request("POST", "/models", body, 400)
		if !strings.Contains(got, "不支持") {
			t.Fatalf("missing rejection explanation: %s", got)
		}
	}
	got := request("POST", "/models", `{"publicId":"Build/company","provider":"grok_build","upstreamModel":"future-text","capability":"responses","enabled":true}`, 201)
	if !strings.Contains(got, `"publicId":"Build/company"`) {
		t.Fatal(got)
	}
	legacy, err := models.Create(ctx, modeldomain.Route{Provider: account.ProviderWeb, PublicID: "legacy", UpstreamModel: "grok-chat-fast", Capability: modeldomain.CapabilityVideo, Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/models/%d", legacy.ID)
	for _, body := range []string{`{"enabled":false}`, `{"enabled":true}`, `{"publicId":"legacy-renamed"}`} {
		got = request("PATCH", path, body, 200)
		if !strings.Contains(got, `"capabilitySupported":false`) || !strings.Contains(got, `"available":false`) {
			t.Fatalf("legacy projection: %s", got)
		}
	}
	got = request("GET", "/models/groups?search=legacy-renamed", "", 200)
	if !strings.Contains(got, `"capabilitySupported":false`) || strings.Contains(got, `"endpointCapabilities":["video"]`) {
		t.Fatalf("legacy group: %s", got)
	}
	request("DELETE", path, "", 200)
}
