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

// TestLegacyPagingRejectsFilterParams 锁定 round 83：兼容分页对 filter/
// 排序参数显式 400（此前静默忽略返回全量）。cursor 分页同参数正常。
func TestLegacyPagingRejectsFilterParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAuditRepository(database)
	if err := repository.CreateBatch(ctx, []auditdomain.Record{
		{RequestID: "lg-one", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", StatusCode: 200, CreatedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	service := auditapp.NewService(repository, slog.Default(), 8, 4, time.Second)
	router := gin.New()
	NewHandler(service).Register(router.Group(""))

	legacy := httptest.NewRequest(http.MethodGet, "/request-audits?errorCode=quality_degraded", nil)
	legacyRec := httptest.NewRecorder()
	router.ServeHTTP(legacyRec, legacy)
	if legacyRec.Code != http.StatusBadRequest || !strings.Contains(legacyRec.Body.String(), "filterRequiresCursor") {
		t.Fatalf("legacy+filter = %d %s, want 400 filterRequiresCursor", legacyRec.Code, legacyRec.Body.String())
	}

	plain := httptest.NewRequest(http.MethodGet, "/request-audits", nil)
	plainRec := httptest.NewRecorder()
	router.ServeHTTP(plainRec, plain)
	if plainRec.Code != http.StatusOK {
		t.Fatalf("legacy 纯分页 = %d, want 200", plainRec.Code)
	}

	cursor := httptest.NewRequest(http.MethodGet, "/request-audits?pagination=cursor&errorCode=quality_degraded", nil)
	cursorRec := httptest.NewRecorder()
	router.ServeHTTP(cursorRec, cursor)
	if cursorRec.Code != http.StatusOK {
		t.Fatalf("cursor+filter = %d, want 200", cursorRec.Code)
	}
}
