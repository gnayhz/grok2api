package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

// The formal management routes, M07 service, real Web adapter and SQL all
// participate. The local upstream installs a replacement before acknowledging
// the old request, so the material race does not depend on scheduling sleeps.
func TestWebProfileHTTPCompletionBelongsToMaterial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, endpoint, replaceAt, failAt string
		failure                           int
		alreadySet, batch, deleted        bool
		status                            int
		terms, birth, nsfw, reauth        bool
		calls                             []string
	}{
		{name: "current terms", endpoint: "accept-terms", status: 200, terms: true, calls: []string{"account-terms", "product-terms"}},
		{name: "old terms", endpoint: "accept-terms", replaceAt: "product-terms", status: 409, calls: []string{"account-terms", "product-terms"}},
		{name: "second terms step failed", endpoint: "accept-terms", failAt: "product-terms", failure: 502, status: 502, calls: []string{"account-terms", "product-terms"}},
		{name: "current birth", endpoint: "birth-date", status: 200, birth: true, calls: []string{"birth"}},
		{name: "old birth", endpoint: "birth-date", replaceAt: "birth", status: 409, calls: []string{"birth"}},
		{name: "current already set birth", endpoint: "nsfw", alreadySet: true, status: 200, birth: true, nsfw: true, calls: []string{"birth", "nsfw"}},
		{name: "old already set birth stops nsfw", endpoint: "nsfw", replaceAt: "birth", alreadySet: true, status: 409, calls: []string{"birth"}},
		{name: "birth rate limit stops nsfw", endpoint: "nsfw", failAt: "birth", failure: 429, status: 502, calls: []string{"birth"}},
		{name: "old nsfw", endpoint: "nsfw", replaceAt: "nsfw", status: 409, calls: []string{"birth", "nsfw"}},
		{name: "current unauthorized", endpoint: "accept-terms", failAt: "account-terms", failure: 401, status: 502, reauth: true, calls: []string{"account-terms"}},
		{name: "old unauthorized", endpoint: "accept-terms", replaceAt: "account-terms", failAt: "account-terms", failure: 401, status: 502, calls: []string{"account-terms"}},
		{name: "delete during success", endpoint: "birth-date", replaceAt: "birth", deleted: true, status: 404, calls: []string{"birth"}},
		{name: "batch old terms stops other steps", batch: true, replaceAt: "product-terms", status: 200, calls: []string{"account-terms", "product-terms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "web-profile-http.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			token, err := cipher.Encrypt("profile-old-sso")
			if err != nil {
				t.Fatal(err)
			}
			v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "profile", SourceKey: "profile", UserID: "old-user", EncryptedAccessToken: token, AuthStatus: account.AuthStatusActive, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			calls := map[string][]string{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				name := map[string]string{
					"/auth_mgmt.AuthManagement/SetTosAcceptedVersion":     "account-terms",
					"/rest/auth/set-tos-accepted":                         "product-terms",
					"/rest/auth/set-birth-date":                           "birth",
					"/auth_mgmt.AuthManagement/UpdateUserFeatureControls": "nsfw",
				}[r.URL.Path]
				cookie, err := r.Cookie("sso")
				if name == "" || err != nil {
					http.Error(w, "unexpected request", 400)
					return
				}
				mu.Lock()
				calls[cookie.Value] = append(calls[cookie.Value], name)
				mu.Unlock()
				if cookie.Value == "profile-old-sso" && name == tc.replaceAt {
					if tc.deleted {
						err = repo.Delete(ctx, v.ID)
					} else {
						replacement := v
						replacement.UserID = "new-user"
						replacement.EncryptedAccessToken, err = cipher.Encrypt("profile-new-sso")
						if err == nil {
							_, _, err = repo.UpsertByIdentity(ctx, replacement)
						}
					}
					if err != nil {
						t.Errorf("replace material: %v", err)
						http.Error(w, "fixture failed", 500)
						return
					}
				}
				if name == tc.failAt {
					w.WriteHeader(tc.failure)
					_, _ = io.WriteString(w, `{"message":"upstream operation failed"}`)
					return
				}
				if name == "birth" && tc.alreadySet {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"code":8,"message":"[WKE=account:birth-date-change-limit-reached]"}`)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)
			statsig := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{'s'}, 70))
			adapter := NewAdapter(Config{BaseURL: upstream.URL, StatsigMode: "manual", StatsigManualValue: statsig}, infraegress.NewManagerWithLimits(egressRepositoryStub{}, cipher, netbudget.Limits{}), cipher, nil, nil)
			adapter.accountsBaseURL = upstream.URL
			service := accountapp.NewService(repo, nil, nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
			router := gin.New()
			accounthttp.NewHandler(accounthttp.Dependencies{Administration: service, Credentials: service, Maintenance: service, Onboarding: accountsyncapp.NewOnboarding(service, service, nil)}).Register(router.Group("/api/admin/v1"))
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			path := fmt.Sprintf("/accounts/web/%d/%s", v.ID, tc.endpoint)
			body := ""
			var peer account.Credential
			if tc.batch {
				peerToken, err := cipher.Encrypt("profile-peer-sso")
				if err != nil {
					t.Fatal(err)
				}
				peer, _, err = repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "peer", SourceKey: "peer", UserID: "peer-user", EncryptedAccessToken: peerToken, AuthStatus: account.AuthStatusActive, Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				path = "/accounts/web/run-scripts"
				body = fmt.Sprintf(`{"ids":["%d","%d"],"actions":{"acceptTerms":true,"enableNSFW":true}}`, v.ID, peer.ID)
			}
			response, err := server.Client().Post(server.URL+"/api/admin/v1"+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.StatusCode != tc.status {
				t.Fatalf("HTTP=%d body=%s err=%v", response.StatusCode, data, readErr)
			}
			if tc.status == 409 && !strings.Contains(string(data), "账号材料已更新") {
				t.Fatalf("conflict lost its actionable cause: %s", data)
			}
			mu.Lock()
			observed := append([]string(nil), calls["profile-old-sso"]...)
			peerCalls := append([]string(nil), calls["profile-peer-sso"]...)
			mu.Unlock()
			if !reflect.DeepEqual(observed, tc.calls) {
				t.Fatalf("calls=%v want=%v", observed, tc.calls)
			}
			current, err := repo.Get(ctx, v.ID)
			if tc.deleted {
				if err == nil {
					t.Fatal("late success resurrected deleted account")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (current.WebTermsAcceptedAt != nil) != tc.terms || (current.WebBirthDateSetAt != nil) != tc.birth || (current.WebNSFWEnabledAt != nil) != tc.nsfw {
				t.Fatalf("wrong persisted markers: terms=%v birth=%v nsfw=%v", current.WebTermsAcceptedAt, current.WebBirthDateSetAt, current.WebNSFWEnabledAt)
			}
			if (current.AuthStatus == account.AuthStatusReauthRequired) != tc.reauth || !current.Enabled {
				t.Fatalf("independent state changed: auth=%s enabled=%v", current.AuthStatus, current.Enabled)
			}
			if tc.replaceAt != "" && (current.CredentialGeneration != v.CredentialGeneration+1 || current.UserID != "new-user") {
				t.Fatal("fixture failed to install the replacement")
			}
			if tc.batch {
				if !strings.Contains(string(data), "event: complete\n") || !strings.Contains(string(data), `"succeeded":1,"failed":1`) || !strings.Contains(string(data), `"completed":2,"total":2`) {
					t.Fatalf("batch progress did not report isolated failure: %s", data)
				}
				storedPeer, err := repo.Get(ctx, peer.ID)
				if err != nil || storedPeer.WebTermsAcceptedAt == nil || storedPeer.WebBirthDateSetAt == nil || storedPeer.WebNSFWEnabledAt == nil || !reflect.DeepEqual(peerCalls, []string{"account-terms", "product-terms", "birth", "nsfw"}) {
					t.Fatalf("batch peer failed: calls=%v err=%v", peerCalls, err)
				}
			}
		})
	}
}
