package cli

import (
	"context"
	"encoding/base64"
	"errors"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrepareImportedCredentialRefreshesRTOnlySeed(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","email":"user@example.com","team_id":"team-1"}`)) + ".signature"
	requests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		wantRefresh := "original-rt"
		response := `{"access_token":"fresh-access","refresh_token":"rotated-rt","id_token":"` + idToken + `","expires_in":3600}`
		if requests == 2 {
			wantRefresh = "rotated-rt"
			response = `{"access_token":"renewed-access","refresh_token":"rotated-again","expires_in":3600}`
		}
		if request.FormValue("grant_type") != "refresh_token" || request.FormValue("refresh_token") != wantRefresh || request.FormValue("client_id") != "custom-client" {
			t.Fatalf("form = %#v", request.Form)
		}
		return oauthResponse(http.StatusOK, response), nil
	})}
	adapter := &Adapter{oauth: newOAuthClient(httpClient, nil, nil), cipher: cipher}
	prepared, err := adapter.PrepareImportedCredential(context.Background(), provider.CredentialSeed{OIDCClientID: "custom-client", RefreshToken: "original-rt"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.AccessToken != "fresh-access" || prepared.RefreshToken != "rotated-rt" || prepared.UserID != "user-1" || prepared.Email != "user@example.com" || prepared.TeamID != "team-1" || prepared.OIDCClientID != "custom-client" || prepared.SourceKey == "" {
		t.Fatalf("prepared seed = %#v", prepared)
	}
	lead := time.Until(prepared.ExpiresAt)
	if lead < 59*time.Minute || lead > 61*time.Minute {
		t.Fatalf("expires in = %s", lead)
	}
	encryptedRefresh, err := cipher.Encrypt(prepared.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := adapter.RefreshCredential(context.Background(), accountdomain.Credential{OIDCClientID: prepared.OIDCClientID, EncryptedRefreshToken: encryptedRefresh})
	if err != nil {
		t.Fatal(err)
	}
	renewedRefresh, err := cipher.Decrypt(renewed.EncryptedRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if renewedRefresh != "rotated-again" || requests != 2 {
		t.Fatalf("renewed refresh = %q, requests = %d", renewedRefresh, requests)
	}
}

func TestCredentialRefreshCallsOAuthAndPersistsRotationEndToEnd(t *testing.T) {
	ctx := context.Background()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if err := request.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "original-rt" || request.Form.Get("client_id") != "custom-client" {
			t.Errorf("form = %#v", request.Form)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"fresh-access","refresh_token":"rotated-rt","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)

	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "oauth-refresh-e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessEncrypted, err := cipher.Encrypt("old-access")
	if err != nil {
		t.Fatal(err)
	}
	refreshEncrypted, err := cipher.Encrypt("original-rt")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{}, cipher)
	adapter.oauth.http = server.Client()
	adapter.oauth.tokenURL = server.URL
	repository := relational.NewAccountRepository(database)
	credential, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "oauth-e2e", SourceKey: "oauth-e2e", OIDCClientID: "custom-client",
		EncryptedAccessToken: accessEncrypted, EncryptedRefreshToken: refreshEncrypted, ExpiresAt: time.Now().UTC().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := accountapp.NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
	refreshed, err := service.EnsureCredential(ctx, credential, true)
	if err != nil {
		t.Fatal(err)
	}
	storedAccess, err := cipher.Decrypt(refreshed.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	storedRefresh, err := cipher.Decrypt(refreshed.EncryptedRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || storedAccess != "fresh-access" || storedRefresh != "rotated-rt" || refreshed.LastRefreshAt == nil {
		t.Fatalf("requests=%d access=%q refresh=%q credential=%#v", requests, storedAccess, storedRefresh, refreshed)
	}
}

func TestOAuthDeviceFlowMatchesOfficialWireContract(t *testing.T) {
	version := "0.2.111"
	requests := 0
	tokenPolls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Header.Get("x-grok-client-version") != version {
			t.Fatalf("client version = %q, want %q", request.Header.Get("x-grok-client-version"), version)
		}
		if request.Header.Get("x-grok-client-surface") != deviceClientSurface {
			t.Fatalf("client surface = %q", request.Header.Get("x-grok-client-surface"))
		}
		if got := request.Header.Get("User-Agent"); got != "grok-shell/"+version+" (linux; x86_64)" {
			t.Fatalf("user agent = %q, want grok-shell/%s (linux; x86_64)", got, version)
		}
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch request.URL.Path {
		case "/oauth2/device/code":
			if request.Form.Get("client_id") != defaultOAuthClientID || request.Form.Get("scope") != defaultOAuthScope || request.Form.Get("referrer") != "grok-build" {
				t.Fatalf("device form = %v", request.Form)
			}
			return oauthResponse(http.StatusOK, `{"device_code":"device","user_code":"ABCD-EFGH","verification_uri":"https://auth.x.ai/activate","verification_uri_complete":"https://auth.x.ai/activate?user_code=ABCD-EFGH","interval":5,"expires_in":1800}`), nil
		case "/oauth2/token":
			if request.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || request.Form.Get("client_id") != defaultOAuthClientID || request.Form.Get("device_code") != "device" {
				t.Fatalf("token form = %v", request.Form)
			}
			tokenPolls++
			if tokenPolls == 1 {
				return oauthResponse(http.StatusBadRequest, `{"error":"authorization_pending"}`), nil
			}
			return oauthResponse(http.StatusOK, `{"access_token":"access","refresh_token":"refresh","id_token":"id","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected OAuth path %q", request.URL.Path)
			return nil, nil
		}
	})}
	client := newOAuthClient(httpClient, func() string { return version }, func() string { return "grok-shell/" + version + " (linux; x86_64)" })

	authorization, err := client.startDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authorization.DeviceCode != "device" || authorization.UserCode != "ABCD-EFGH" || authorization.Interval != 5*time.Second || authorization.ExpiresIn != 30*time.Minute {
		t.Fatalf("authorization = %#v", authorization)
	}

	version = "0.2.112"
	if _, err := client.pollDevice(context.Background(), authorization.DeviceCode); !errors.Is(err, provider.ErrAuthorizationPending) {
		t.Fatalf("poll error = %v", err)
	}
	tokens, err := client.pollDevice(context.Background(), authorization.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "refresh" || tokens.IDToken != "id" || time.Until(tokens.ExpiresAt) < 59*time.Minute {
		t.Fatalf("tokens = %#v", tokens)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

func TestNewAdapterOAuthUserAgentFallsBackToRecommended(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	// 空白 UserAgent(管理员清空设置)不得退回 Go-http-client/<version> 默认标识。
	adapter := NewAdapter(Config{ClientVersion: "1.0.4"}, cipher)
	request, requestErr := http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token", nil)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	adapter.oauth.applyUserAgent(request)
	if got := request.Header.Get("User-Agent"); got != config.RecommendedBuildUserAgent {
		t.Fatalf("fallback user agent = %q, want %q", got, config.RecommendedBuildUserAgent)
	}

	adapter.UpdateConfig(Config{ClientVersion: "1.0.4", UserAgent: "grok-shell/0.2.200 (macos; aarch64)"})
	request, requestErr = http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token", nil)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	adapter.oauth.applyUserAgent(request)
	if got := request.Header.Get("User-Agent"); got != "grok-shell/0.2.200 (macos; aarch64)" {
		t.Fatalf("configured user agent = %q", got)
	}
}

func oauthResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestOAuthRefreshClassifiesPermanentAndTransientFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		permanent  bool
		code       string
		message    string
		response   string
	}{
		{name: "transient upstream", status: http.StatusServiceUnavailable, body: `{"error":"temporarily_unavailable"}`, retryAfter: "7", code: "temporarily_unavailable"},
		{name: "bad request is not inherently permanent", status: http.StatusBadRequest, body: `{"error":"temporarily_unavailable","error_description":"Try another egress"}`, code: "temporarily_unavailable", message: "Try another egress"},
		{name: "unauthorized client is configuration scoped", status: http.StatusUnauthorized, body: `{"error":"invalid_client","error_description":"Client authentication failed"}`, code: "invalid_client", message: "Client authentication failed"},
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"Refresh token has expired","message":"Access denied","request_id":"req-123","refresh_token":"must-not-leak"}`, permanent: true, code: "invalid_grant", message: "Refresh token has expired · Access denied", response: `"refresh_token":"[REDACTED]"`},
		{name: "nested error", status: http.StatusBadRequest, body: `{"error":{"code":"invalid_client","message":"Client rejected","detail":"Application disabled"}}`, code: "invalid_client", message: "Client rejected · Application disabled", response: `"detail":"Application disabled"`},
		{name: "explicit revoked refresh token", status: http.StatusUnauthorized, body: `{"error":"refresh_token_revoked"}`, permanent: true, code: "refresh_token_revoked"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("x-grok-client-version") != "" || request.Header.Get("x-grok-client-surface") != "" {
					t.Fatalf("refresh request unexpectedly included device headers: %v", request.Header)
				}
				if got := request.Header.Get("User-Agent"); got != "grok-shell/0.2.111 (linux; x86_64)" {
					t.Fatalf("refresh user agent = %q", got)
				}
				if request.FormValue("grant_type") != "refresh_token" || request.FormValue("refresh_token") != "refresh" {
					t.Fatalf("form = %#v", request.Form)
				}
				header := make(http.Header)
				if test.retryAfter != "" {
					header.Set("Retry-After", test.retryAfter)
				}
				return &http.Response{StatusCode: test.status, Header: header, Body: io.NopCloser(strings.NewReader(test.body)), Request: request}, nil
			})}
			client := newOAuthClient(httpClient, func() string { return "0.2.111" }, func() string { return "grok-shell/0.2.111 (linux; x86_64)" })
			client.tokenURL = "https://auth.x.ai/oauth2/token"
			_, err := client.refresh(context.Background(), "refresh")
			var refreshErr *provider.CredentialRefreshError
			if !errors.As(err, &refreshErr) || refreshErr.Status != test.status || refreshErr.Permanent != test.permanent || refreshErr.Code != test.code || refreshErr.Message != test.message {
				t.Fatalf("error = %#v", err)
			}
			if test.response != "" && !strings.Contains(refreshErr.Response, test.response) {
				t.Fatalf("response = %q, want substring %q", refreshErr.Response, test.response)
			}
			if strings.Contains(refreshErr.Response, "must-not-leak") {
				t.Fatalf("response leaked refresh token: %q", refreshErr.Response)
			}
			if test.retryAfter != "" && refreshErr.RetryAfter != 7*time.Second {
				t.Fatalf("retry after = %s", refreshErr.RetryAfter)
			}
		})
	}
}

func TestOAuthRefreshTreatsMalformedSuccessAsRetryable(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return oauthResponse(http.StatusOK, `{"expires_in":3600}`), nil
	})}
	client := newOAuthClient(httpClient, nil, nil)
	_, err := client.refresh(context.Background(), "refresh")
	var refreshErr *provider.CredentialRefreshError
	if !errors.As(err, &refreshErr) || refreshErr.Code != "missing_access_token" || refreshErr.Permanent {
		t.Fatalf("error = %#v", err)
	}
}

func TestOAuthScopeMatchesOfficialPersonalAccountContract(t *testing.T) {
	values := strings.Fields(defaultOAuthScope)
	want := []string{
		"openid", "profile", "email", "offline_access", "grok-cli:access", "api:access",
		"conversations:read", "conversations:write", "workspaces:read", "workspaces:write",
	}
	if len(values) != len(want) {
		t.Fatalf("scope count = %d, want %d: %v", len(values), len(want), values)
	}
	for index := range want {
		if values[index] != want[index] {
			t.Fatalf("scope[%d] = %q, want %q", index, values[index], want[index])
		}
	}
}

// The old request has already sent its original RT when another connection
// replaces local material. Exercise Provider parsing/encryption and M07/SQL.
func TestOAuthInFlightCompletionKeepsImportedCredential(t *testing.T) {
	for _, mode := range []string{"success_after_import", "failure_after_import", "new_generation_refresh", "cancel_after_response"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "inflight.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			encrypt := func(v string) string {
				t.Helper()
				s, err := cipher.Encrypt(v)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			repo := relational.NewAccountRepository(database)
			original, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "inflight", SourceKey: "inflight", OIDCClientID: "custom-client", EncryptedAccessToken: encrypt("old-access"), EncryptedRefreshToken: encrypt("original-rt"), ExpiresAt: time.Now().Add(-time.Minute), Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			defer finish()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if mode == "new_generation_refresh" && r.Form.Get("refresh_token") == "imported-refresh" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"access_token":"fresh-imported-access","refresh_token":"fresh-imported-refresh","expires_in":3600}`)
					return
				}
				if r.Form.Get("refresh_token") != "original-rt" || r.Form.Get("client_id") != "custom-client" {
					t.Error("wrong observed credential on wire")
				}
				close(entered)
				<-release
				w.Header().Set("Content-Type", "application/json")
				if mode == "failure_after_import" || mode == "new_generation_refresh" {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"old refresh rejected"}`)
					return
				}
				_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"rotated-rt","expires_in":3600}`)
			}))
			defer func() { finish(); server.Close() }()
			adapter := NewAdapter(Config{}, cipher)
			adapter.oauth.tokenURL = server.URL
			adapter.oauth.http = server.Client()
			if mode == "cancel_after_response" {
				adapter.oauth.http.Transport = cancelOAuthBodyTransport{base: server.Client().Transport, cancel: cancel}
			}
			service := accountapp.NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
			type outcome struct {
				credential accountdomain.Credential
				err        error
			}
			done := make(chan outcome, 1)
			go func() { v, err := service.EnsureCredential(requestCtx, original, true); done <- outcome{v, err} }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("OAuth request did not start")
			}
			expectedGeneration := original.CredentialGeneration + 1
			if mode != "cancel_after_response" {
				replacement := original
				replacement.EncryptedAccessToken, replacement.EncryptedRefreshToken = encrypt("imported-access"), encrypt("imported-refresh")
				replacement.ExpiresAt = time.Now().Add(time.Hour)
				if _, _, err := repo.UpsertByIdentity(ctx, replacement); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "new_generation_refresh" {
				current, err := repo.Get(ctx, original.ID)
				if err != nil {
					t.Fatal(err)
				}
				newDone := make(chan outcome, 1)
				go func() { v, err := service.EnsureCredential(ctx, current, true); newDone <- outcome{v, err} }()
				select {
				case next := <-newDone:
					if next.err != nil || next.credential.CredentialGeneration != current.CredentialGeneration+1 {
						t.Fatal("new generation did not perform its own refresh")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("new generation joined old in-flight refresh")
				}
				expectedGeneration++
			}
			finish()
			var result outcome
			select {
			case result = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("OAuth completion did not return")
			}
			if mode == "failure_after_import" || mode == "new_generation_refresh" {
				if result.err == nil {
					t.Fatal("old operation lost its own error")
				}
			} else if result.err != nil {
				t.Fatal(result.err)
			}
			stored, err := repo.Get(ctx, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			access, err := cipher.Decrypt(stored.EncryptedAccessToken)
			if err != nil {
				t.Fatal(err)
			}
			refresh, err := cipher.Decrypt(stored.EncryptedRefreshToken)
			if err != nil {
				t.Fatal(err)
			}
			wantAccess, wantRefresh := "imported-access", "imported-refresh"
			if mode == "cancel_after_response" {
				wantAccess, wantRefresh = "fresh-access", "rotated-rt"
				if requestCtx.Err() == nil {
					t.Fatal("request was not canceled before persistence")
				}
			}
			wantRequests := int32(1)
			if mode == "new_generation_refresh" {
				wantAccess, wantRefresh, wantRequests = "fresh-imported-access", "fresh-imported-refresh", 2
			}
			if access != wantAccess || refresh != wantRefresh || stored.CredentialGeneration != expectedGeneration || stored.AuthStatus != accountdomain.AuthStatusActive || stored.RefreshPermanent || stored.RefreshFailureCount != 0 || requests.Load() != wantRequests {
				t.Fatalf("completion damaged replacement: generation=%d auth=%s failures=%d", stored.CredentialGeneration, stored.AuthStatus, stored.RefreshFailureCount)
			}
			if mode == "success_after_import" && result.credential.CredentialGeneration != expectedGeneration {
				t.Fatal("caller received obsolete success material")
			}
		})
	}
}

type cancelOAuthBodyTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelOAuthBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := c.base.RoundTrip(r)
	if err == nil {
		response.Body = &cancelOAuthBody{ReadCloser: response.Body, cancel: c.cancel}
	}
	return response, err
}

type cancelOAuthBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOAuthBody) Close() error { err := c.ReadCloser.Close(); c.cancel(); return err }
