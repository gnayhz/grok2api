package account

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

// TestUpdateRiskStatusContract：PATCH /accounts/:id 的 riskStatus 合同（D 审查缺口）
// —— set/clear/omit 保持原值/非法值拒绝且不改写存量值。
func TestUpdateRiskStatusContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "risk-status-contract.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	audits := relational.NewAuditRepository(database)
	service := accountapp.NewService(repo, audits, nil, nil, nil, nil, nil)
	handler := NewHandler(service, nil)

	created, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "contract", SourceKey: "contract",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatUint(created.ID, 10)

	patch := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Params = []gin.Param{{Key: "id", Value: id}}
		ginContext.Request = httptest.NewRequest("PATCH", "/api/admin/v1/accounts/"+id, strings.NewReader(body))
		ginContext.Request.Header.Set("Content-Type", "application/json")
		handler.update(ginContext)
		return recorder
	}

	if rec := patch(`{"riskStatus":"rsc_denied"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"riskStatus":"rsc_denied"`) {
		t.Fatalf("set: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := patch(`{"riskStatus":"bogus"}`); rec.Code == 200 || !strings.Contains(rec.Body.String(), "riskStatus") {
		t.Fatalf("invalid: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RiskStatus != accountdomain.RiskStatusRSCDenied {
		t.Fatalf("invalid write mutated state: %q", stored.RiskStatus)
	}
	if rec := patch(`{"priority":2}`); rec.Code != 200 {
		t.Fatalf("omit: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err = repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RiskStatus != accountdomain.RiskStatusRSCDenied {
		t.Fatalf("omit must keep riskStatus, got %q", stored.RiskStatus)
	}
	if rec := patch(`{"riskStatus":""}`); rec.Code != 200 {
		t.Fatalf("clear: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err = repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RiskStatus != "" {
		t.Fatalf("clear failed: %q", stored.RiskStatus)
	}
}
