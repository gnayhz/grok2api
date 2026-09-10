package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

type conversionHTTPClient struct {
	local      *url.URL
	client     *http.Client
	afterToken func()
}

func (c conversionHTTPClient) Do(r *http.Request) (*http.Response, error) {
	request := r.Clone(r.Context())
	next := *r.URL
	next.Scheme, next.Host = c.local.Scheme, c.local.Host
	request.URL = &next
	response, err := c.client.Do(request)
	if err == nil && r.URL.Path == "/oauth2/token" && c.afterToken != nil {
		response.Body = conversionHTTPBody{ReadCloser: response.Body, after: c.afterToken}
	}
	return response, err
}

type conversionHTTPBody struct {
	io.ReadCloser
	after func()
}

func (b conversionHTTPBody) Close() error { err := b.ReadCloser.Close(); b.after(); return err }

type conversionHTTPAdapter struct{ flow *ssoBuildFlow }

func (*conversionHTTPAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *conversionHTTPAdapter) ConvertToBuild(ctx context.Context, c account.Credential) (provider.CredentialSeed, error) {
	return a.flow.convert(ctx, c)
}

type conversionHTTPPort struct {
	repository.AccountRepository
	after func()
}

func (p *conversionHTTPPort) ImportAccounts(ctx context.Context, inputs []repository.AccountImport) ([]repository.AccountUpsertResult, error) {
	out, err := p.AccountRepository.ImportAccounts(ctx, inputs)
	if err == nil && p.after != nil {
		f := p.after
		p.after = nil
		f()
	}
	return out, err
}

func TestConversionFormalHTTPWithRealOAuthFlow(t *testing.T) {
	for _, scenario := range []string{"success", "cancel_known_grant", "source_changed_after_install", "source_deleted_before_install"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "conversion-http.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			token, err := cipher.Encrypt("local-sso")
			if err != nil {
				t.Fatal(err)
			}
			web, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web", SourceKey: "web", EncryptedAccessToken: token, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				cookie, err := r.Cookie("sso")
				if err != nil || cookie.Value != "local-sso" {
					t.Error("SSO cookie not carried through flow")
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				switch r.URL.Path {
				case "/oauth2/device/code":
					_, _ = io.WriteString(w, `{"device_code":"dc","user_code":"uc","interval":1,"expires_in":60}`)
				case "/oauth2/device/verify":
					http.Redirect(w, r, "https://accounts.x.ai/oauth2/device/consent", 303)
				case "/oauth2/device/approve":
					http.Redirect(w, r, "https://accounts.x.ai/oauth2/device/done", 303)
				case "/oauth2/token":
					if r.Form.Get("device_code") != "dc" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
						t.Error("conversion token form changed")
					}
					_, _ = io.WriteString(w, `{"access_token":"converted-access","refresh_token":"converted-refresh","expires_in":3600}`)
				default:
					t.Error("flow followed the result-page redirect")
					http.Error(w, "unexpected", 500)
				}
			}))
			defer upstream.Close()
			local, _ := url.Parse(upstream.URL)
			client := upstream.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			rewrite := conversionHTTPClient{local: local, client: client}
			port := &conversionHTTPPort{AccountRepository: repo}
			if scenario == "cancel_known_grant" {
				rewrite.afterToken = cancel
			}
			if scenario == "source_deleted_before_install" {
				rewrite.afterToken = func() {
					if err := repo.Delete(context.Background(), web.ID); err != nil {
						t.Error(err)
					}
				}
			}
			if scenario == "source_changed_after_install" {
				port.after = func() {
					web.EncryptedAccessToken = "replacement"
					if _, _, err := repo.UpsertByIdentity(context.Background(), web); err != nil {
						t.Error(err)
					}
				}
			}
			adapter := &conversionHTTPAdapter{flow: &ssoBuildFlow{client: rewrite, userAgent: "local-test", cookies: map[string]string{"sso": "local-sso", "sso-rw": "local-sso"}}}
			service := accountapp.NewService(port, relational.NewAuditRepository(db), nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())
			router := gin.New()
			accounthttp.NewHandler(service, nil).Register(router.Group("/api/admin/v1"))
			request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/accounts/web/convert-to-build", strings.NewReader(fmt.Sprintf(`{"ids":["%d"],"strategy":"all"}`, web.ID))).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != 200 || calls.Load() != 4 {
				t.Fatalf("conversion HTTP=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
			values, err := repo.ListEnabled(context.Background(), account.ProviderBuild)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "source_deleted_before_install" {
				if len(values) != 0 {
					t.Fatal("deleted source restored by grant")
				}
				return
			}
			if len(values) != 1 {
				t.Fatalf("known grant count=%d body=%s", len(values), response.Body.String())
			}
			values[0], err = repo.Get(context.Background(), values[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			material, err := cipher.Decrypt(values[0].EncryptedRefreshToken)
			if err != nil || material != "converted-refresh" {
				t.Fatalf("grant persistence: %v", err)
			}
			if scenario == "source_changed_after_install" {
				if values[0].LinkedAccountID != 0 || !strings.Contains(response.Body.String(), `"failed":1`) {
					t.Fatalf("stale conversion linked/reported success: %s", response.Body.String())
				}
			} else if values[0].LinkedAccountID != web.ID {
				t.Fatal("known grant relation not completed")
			}
		})
	}
}
