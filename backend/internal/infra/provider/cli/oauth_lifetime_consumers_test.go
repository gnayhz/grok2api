package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestHTTPBuildOAuthLifetimeRetainsGrantAndFutureDeadlines(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		for _, field := range []struct {
			name   string
			normal int64
		}{{"token_expires", 3600}, {"device_expires", 1800}, {"device_interval", 5}} {
			for _, seconds := range []int64{field.normal, 1 << 62, math.MaxInt64} {
				t.Run(fmt.Sprintf("%s/%s/%d", driver, field.name, seconds), func(t *testing.T) {
					f := newControlDocumentFixture(t, driver, "refresh")
					var issued atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						issued.Add(1)
						w.Header().Set("Content-Type", "application/json")
						if field.name == "token_expires" {
							_, _ = fmt.Fprintf(w, `{"access_token":"issued-access","refresh_token":"issued-refresh","expires_in":%d}`, seconds)
						} else {
							expires, interval := int64(1800), int64(5)
							if field.name == "device_expires" {
								expires = seconds
							} else {
								interval = seconds
							}
							_, _ = fmt.Fprintf(w, `{"device_code":"issued-device","user_code":"ABCD","verification_uri":"https://example.test/device","expires_in":%d,"interval":%d}`, expires, interval)
						}
					}))
					t.Cleanup(server.Close)
					f.adapter.oauth.tokenURL, f.adapter.oauth.deviceURL = server.URL, server.URL
					ctx := context.Background()
					before, err := f.accounts.Get(ctx, f.accountID)
					if err != nil {
						t.Fatal(err)
					}
					started := time.Now()
					want := time.Duration(math.MaxInt64)
					if seconds == field.normal {
						want = time.Duration(field.normal) * time.Second
					}
					if field.name == "token_expires" {
						output := f.request(http.MethodPost, fmt.Sprintf("/accounts/%d/refresh-token", f.accountID))
						if output.Code != 200 {
							t.Fatalf("complete grant was rejected: status=%d", output.Code)
						}
						stored, err := f.accounts.Get(ctx, f.accountID)
						if err != nil {
							t.Fatal(err)
						}
						access, accessErr := f.adapter.cipher.Decrypt(stored.EncryptedAccessToken)
						refresh, refreshErr := f.adapter.cipher.Decrypt(stored.EncryptedRefreshToken)
						if accessErr != nil || refreshErr != nil || access != "issued-access" || refresh != "issued-refresh" || stored.CredentialRef() == before.CredentialRef() || stored.AuthStatus != account.AuthStatusActive {
							t.Fatal("issued rotated credentials were not retained")
						}
						if duration := stored.ExpiresAt.Sub(started); duration < want || duration-want > time.Since(started)+time.Millisecond {
							t.Fatalf("persisted token lifetime=%v want=%v", duration, want)
						}
					} else {
						output := f.request(http.MethodPost, "/accounts/device/start")
						var envelope struct {
							Data struct {
								SessionID       string `json:"sessionId"`
								IntervalSeconds int64  `json:"intervalSeconds"`
							} `json:"data"`
						}
						if err := json.Unmarshal(output.Body.Bytes(), &envelope); err != nil || output.Code != 201 {
							t.Fatalf("device response status=%d err=%v", output.Code, err)
						}
						session, err := f.sessions.Get(ctx, envelope.Data.SessionID, time.Now())
						if err != nil || session.DeviceCode != "issued-device" {
							t.Fatalf("issued device session was already expired: %v", err)
						}
						duration := session.ExpiresAt.Sub(started)
						if field.name == "device_interval" {
							duration = session.Interval
							if session.NextPollAt.Sub(started) < want || envelope.Data.IntervalSeconds < int64(want/time.Second) {
								t.Fatal("device polling deadline or displayed interval wrapped")
							}
						}
						if duration < want || duration-want > time.Since(started)+time.Millisecond {
							t.Fatalf("device lifetime=%v want=%v", duration, want)
						}
					}
					if issued.Load() != 1 || f.generated.Load() != 0 {
						t.Fatal("lifetime handling retried issuance or generated inference")
					}
				})
			}
		}
	}
}
