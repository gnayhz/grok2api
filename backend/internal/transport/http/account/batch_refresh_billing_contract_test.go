package account

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

// TestBatchRefreshBillingContract：POST /accounts/batch/refresh-billing 的
// 契约——仅 Grok Build 号池（其他 provider 400 invalidProvider；跨池账号
// 409 accountPoolMismatch），空/非法 ids 400；有效 Build 账号进入批量执行
// 并返回 succeeded/failed 计数（无注册 Billing 适配器时按失败计，
// 不拖垮整个批次）。
func TestBatchRefreshBillingContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "batch-billing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	audits := relational.NewAuditRepository(database)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	// 空 Provider 注册表：Billing 适配器缺失 → 单账号失败计数，路由仍 200。
	service := accountapp.NewService(repo, audits, nil, nil, provider.NewRegistry(), cipher, nil)
	handler := NewHandler(service, nil)

	build, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "b1", SourceKey: "b1",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, Name: "w1", SourceKey: "w1",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	buildID := strconv.FormatUint(build.ID, 10)
	webID := strconv.FormatUint(web.ID, 10)

	post := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/v1/accounts/batch/refresh-billing", strings.NewReader(body))
		ginContext.Request.Header.Set("Content-Type", "application/json")
		handler.batchRefreshBilling(ginContext)
		return recorder
	}

	if rec := post(`{"ids":["` + buildID + `"],"provider":"grok_web"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-build provider must 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"ids":["` + webID + `"],"provider":"grok_build"}`); rec.Code != http.StatusConflict {
		t.Fatalf("cross-pool ids must 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"ids":["abc"],"provider":"grok_build"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed ids must 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec := post(`{"ids":["` + buildID + `"],"provider":"grok_build"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid build batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data struct {
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if payload.Data.Succeeded != 0 || payload.Data.Failed != 1 {
		t.Fatalf("unregistered billing adapter must count as failed, got succeeded=%d failed=%d", payload.Data.Succeeded, payload.Data.Failed)
	}
}
