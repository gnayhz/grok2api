package cli

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestBuildControlDocumentsRequireCompleteSuccess(t *testing.T) {
	for _, endpoint := range []struct {
		name, path, body string
		limit            int
	}{
		{"models", "/v1/models", `{"data":[{"id":"grok-4.5"}]}`, 4 << 20},
		{"billing", "/v1/billing", `{"config":{"creditUsagePercent":10}}`, 2 << 20},
		{"subscription", "/v1/user", `{"subscriptionTier":"SuperGrokPro"}`, 1 << 20},
		{"refresh", "/token", `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`, 1 << 20},
		{"device", "/device", `{"device_code":"local-device","user_code":"ABCD","verification_uri":"https://example.test/device","expires_in":1800,"interval":5}`, 1 << 20},
	} {
		for _, name := range []string{"normal", "exact_limit", "one_extra_space", "oversize_bad_tail", "gzip_bad_tail", "short_content_length", "cancel"} {
			malformed := name != "normal" && name != "exact_limit"
			t.Run(endpoint.name+"/"+name, func(t *testing.T) {
				body := endpoint.body
				if name == "exact_limit" || name == "one_extra_space" || name == "oversize_bad_tail" || name == "gzip_bad_tail" {
					body += strings.Repeat(" ", endpoint.limit-len(body))
				}
				if name == "one_extra_space" {
					body += " "
				} else if name == "oversize_bad_tail" || name == "gzip_bad_tail" {
					body += "{"
				}
				bodySent := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != endpoint.path {
						// GetBilling also samples the optional live subscription.
						if endpoint.name == "billing" && r.URL.Path == "/v1/user" {
							_, _ = io.WriteString(w, `{"subscriptionTier":"Free"}`)
							return
						}
						http.Error(w, "unexpected endpoint", 400)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if name == "gzip_bad_tail" {
						w.Header().Set("Content-Encoding", "gzip")
						compressed := gzip.NewWriter(w)
						_, _ = io.WriteString(compressed, body)
						_ = compressed.Close()
						return
					}
					if name == "short_content_length" {
						w.Header().Set("Content-Length", strconv.Itoa(len(body)+100))
					}
					_, _ = io.WriteString(w, body)
					if name == "cancel" {
						w.(http.Flusher).Flush()
						close(bodySent)
						<-r.Context().Done()
					}
				}))
				t.Cleanup(server.Close)
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					t.Fatal(err)
				}
				encrypted, err := cipher.Encrypt("original-credential")
				if err != nil {
					t.Fatal(err)
				}
				adapter := NewAdapter(Config{BaseURL: server.URL + "/v1"}, cipher)
				t.Cleanup(adapter.base.current.Load().CloseIdleConnections)
				adapter.oauth.tokenURL = server.URL + "/token"
				adapter.oauth.deviceURL = server.URL + "/device"
				credential := account.Credential{ID: 7, Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: encrypted, EncryptedRefreshToken: encrypted}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if name == "cancel" {
					go func() {
						select {
						case <-bodySent:
							cancel()
						case <-ctx.Done():
						}
					}()
				}
				switch endpoint.name {
				case "models":
					_, err = adapter.ListModels(ctx, credential)
				case "billing":
					_, err = adapter.GetBilling(ctx, credential)
				case "subscription":
					_, err = adapter.getSubscriptionTier(ctx, credential, "original-credential")
				case "refresh":
					_, err = adapter.RefreshCredential(ctx, credential)
				case "device":
					_, err = adapter.StartDeviceAuthorization(ctx)
				}
				if malformed && err == nil {
					t.Fatal("accepted an incomplete successful control document")
				}
				if malformed && endpoint.name == "refresh" {
					var refreshErr *provider.CredentialRefreshError
					if errors.As(err, &refreshErr) && refreshErr.Permanent {
						t.Fatal("invalid successful document permanently rejected the credential")
					}
				}
				if !malformed && err != nil {
					t.Fatalf("normal response failed: %v", err)
				}
			})
		}
	}
}
