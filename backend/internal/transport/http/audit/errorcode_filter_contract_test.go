package audit

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

// TestRequestAuditErrorCodeFilterContract exercises the errorCode filter at the
// full HTTP stack: routing, query parsing, service+repository SQL, and response
// shaping. Seeded records mix quality_degraded with other codes and none; only
// the matching subset may return.
func TestRequestAuditErrorCodeFilterContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "errorcode-filter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	if err := repository.CreateBatch(ctx, []auditdomain.Record{
		{RequestID: "qd-one", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", StatusCode: 503, ErrorCode: "quality_degraded", CreatedAt: now},
		{RequestID: "qd-two", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", StatusCode: 503, ErrorCode: "quality_degraded", CreatedAt: now.Add(-time.Second)},
		{RequestID: "other-code", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", StatusCode: 500, ErrorCode: "upstream_server_error", CreatedAt: now.Add(-2 * time.Second)},
		{RequestID: "no-code", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", StatusCode: 200, CreatedAt: now.Add(-3 * time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	service := auditapp.NewService(repository, slog.Default(), 8, 4, time.Second)
	router := gin.New()
	NewHandler(service).Register(router.Group(""))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/request-audits?pagination=cursor&pageSize=10&errorCode=quality_degraded", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "qd-one") || !strings.Contains(body, "qd-two") {
		t.Fatalf("matching records missing: %s", body)
	}
	if strings.Contains(body, "other-code") || strings.Contains(body, "no-code") {
		t.Fatalf("non-matching records leaked through the errorCode filter: %s", body)
	}
}
