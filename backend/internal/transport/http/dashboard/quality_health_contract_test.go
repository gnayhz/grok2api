package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	"github.com/gin-gonic/gin"
)

// TestHandlerReturnsQualityHealthResources locks the transport contract for
// the quality-health resource fields: cooldownAccounts, riskAccounts, and
// qualityDegradedRequests must round-trip through the JSON envelope so the
// admin dashboard card reflects schedulable reality (risk-flagged accounts
// are never schedulable).
func TestHandlerReturnsQualityHealthResources(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := dashboardapp.NewService(&dashboardRepositoryStub{aggregate: dashboarddomain.Aggregate{
		Resources: dashboarddomain.Resources{ActiveAccounts: 4, TotalAccounts: 11, CooldownAccounts: 2, RiskAccounts: 7, QualityDegradedRequests: 42},
	}})
	router := gin.New()
	NewHandler(service).Register(router.Group("/api/admin/v1"))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/dashboard?period=24h", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data struct {
			Resources struct {
				ActiveAccounts          int64 `json:"activeAccounts"`
				CooldownAccounts        int64 `json:"cooldownAccounts"`
				RiskAccounts            int64 `json:"riskAccounts"`
				QualityDegradedRequests int64 `json:"qualityDegradedRequests"`
			} `json:"resources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	resources := envelope.Data.Resources
	if resources.ActiveAccounts != 4 || resources.CooldownAccounts != 2 || resources.RiskAccounts != 7 || resources.QualityDegradedRequests != 42 {
		t.Fatalf("quality-health resources = %#v", resources)
	}
}
