package account

import (
	"context"
	"encoding/base64"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func TestBillingResetConflictIsVisibleOverHTTP(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "billing-conflict.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	token, _ := cipher.Encrypt("synthetic")
	v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, Name: "conflict", SourceKey: "conflict", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/billing") {
			if err := repo.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, `{"monthlyLimit":100,"used":0}`)
		} else {
			fmt.Fprint(w, `{"subscriptionTier":"SuperGrok"}`)
		}
	}))
	defer upstream.Close()
	service := accountapp.NewService(repo, nil, nil, nil, providerimpl.NewRegistry(cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)), cipher, security.RandomTokenSource{}, nil, nil, nil)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	newTestHandler(service, nil).Register(router.Group("/api/admin/v1"))
	server := httptest.NewServer(router)
	defer server.Close()
	response, err := http.Post(fmt.Sprintf("%s/api/admin/v1/accounts/%d/refresh-billing", server.URL, v.ID), "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict || !strings.Contains(string(body), "billingRefreshFailed") || !strings.Contains(string(body), "额度状态已更新") {
		t.Fatalf("conflict contract: %d %s", response.StatusCode, body)
	}
}
