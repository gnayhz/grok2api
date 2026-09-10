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
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func TestAdminCookieAndRiskHTTPContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "admin-cookie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web", EncryptedAccessToken: "sso", Enabled: true, AuthStatus: accountdomain.AuthStatusActive, WebTier: accountdomain.WebTierSuper})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, nil, cipher, nil), nil)
	patch := func(body string, status int) accountdomain.Credential {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Params = []gin.Param{{Key: "id", Value: strconv.FormatUint(v.ID, 10)}}
		c.Request = httptest.NewRequest("PATCH", "/api/admin/v1/accounts/"+strconv.FormatUint(v.ID, 10), strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.update(c)
		if rec.Code != status {
			t.Fatalf("status=%d want=%d body=%s", rec.Code, status, rec.Body.String())
		}
		stored, err := repo.Get(ctx, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.EncryptedAccessToken != v.EncryptedAccessToken || stored.WebTier != v.WebTier || stored.AuthStatus != v.AuthStatus {
			t.Fatal("admin HTTP changed credential/profile/auth state")
		}
		return stored
	}
	stored := patch(`{"name":"  renamed  ","cloudflareCookies":"cf_clearance=synthetic-clearance; ignored=discarded","riskStatus":"rsc_denied","priority":0}`, 200)
	cookie, err := cipher.Decrypt(stored.EncryptedCloudflareCookie)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "renamed" || stored.Priority != 0 || stored.RiskTrigger != accountdomain.RiskTriggerManual || cookie != "cf_clearance=synthetic-clearance" {
		t.Fatal("admin inputs were not normalized and committed")
	}
	previous := stored.EncryptedCloudflareCookie
	for _, body := range []string{
		`{"name":"invalid","cloudflareCookies":"cf_clearance=new","riskStatus":"" ,"maxConcurrent":0}`,
		`{"name":"invalid","cloudflareCookies":"ignored=discarded","riskStatus":""}`,
		`{"name":"invalid","cloudflareCookies":"cf_clearance=new","riskStatus":"", "buildSuperEntitled":true}`,
	} {
		stored = patch(body, 400)
		if stored.Name != "renamed" || stored.EncryptedCloudflareCookie != previous || stored.RiskStatus != accountdomain.RiskStatusRSCDenied {
			t.Fatal("invalid compound request partially changed state")
		}
	}
	stored = patch(`{"cloudflareCookies":"   "}`, 200)
	if stored.EncryptedCloudflareCookie != previous {
		t.Fatal("blank cookie field unexpectedly cleared stored material")
	}
	stored = patch(`{"clearCloudflareCookies":true,"cloudflareCookies":"ignored=discarded","riskStatus":""}`, 200)
	if stored.EncryptedCloudflareCookie != "" || stored.RiskStatus != "" || stored.RiskTrigger != "" {
		t.Fatal("explicit clear or its precedence changed")
	}
}
