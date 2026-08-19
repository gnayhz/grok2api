package egress

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// The stats endpoint is exercised through the real handler registration path
// so the route wiring itself is under test, not a re-declared copy.
func TestRouteRuleStatsRegisteredRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/api/admin/v1")
	// A nil-service handler is acceptable: the stats endpoint never touches it.
	handler := &Handler{}
	handler.Register(group)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/v1/egress-operations/route-rule-stats", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (read-only stats must not require a service)", recorder.Code)
	}
}
