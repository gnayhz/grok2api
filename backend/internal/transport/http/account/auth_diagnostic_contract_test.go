package account

import (
	"context"
	"encoding/json"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAccountAuthDiagnosticHTTPContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth-detail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "auth-contract", SourceKey: "auth-contract", EncryptedAccessToken: "synthetic-secret", AuthStatus: accountdomain.AuthStatusActive, LastError: accountdomain.LastErrorMissingThinking, FailureCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, nil, nil, security.RandomTokenSource{}, nil, nil, nil)
	if err := service.MarkReauthRequired(ctx, v.CredentialRef(), "Grok Web SSO credential rejected"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: strconv.FormatUint(v.ID, 10)}}
	c.Request = httptest.NewRequest("GET", "/api/admin/v1/accounts/"+strconv.FormatUint(v.ID, 10), nil)
	newTestHandler(service, nil).get(c)
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || payload.Data["authStatus"] != "reauthRequired" || payload.Data["authError"] != "Grok Web SSO credential rejected" || payload.Data["lastError"] != accountdomain.LastErrorMissingThinking || payload.Data["failureCount"] != float64(3) {
		t.Fatalf("auth/health contract: HTTP %d %s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{"credentialGeneration", "encryptedAccessToken", "encryptedRefreshToken"} {
		if _, ok := payload.Data[key]; ok {
			t.Fatalf("internal material leaked through %s", key)
		}
	}
	if path := os.Getenv("GROK_TEST_ACCOUNT_AUTH_FIXTURE"); path != "" {
		if err := os.WriteFile(path, rec.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
