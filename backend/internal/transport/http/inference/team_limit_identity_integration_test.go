package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestHTTPObservedTeamLimitUsesCurrentCredentialGeneration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"responses", "chat/completions", "messages"} {
			for _, stream := range []bool{false, true} {
				for _, action := range []string{"original", "replacement", "same_team_rotation"} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", dialect, operation, stream, action), func(t *testing.T) {
						var calls atomic.Int32
						f := newResponseRetentionFixture(t, dialect, account.ProviderBuild, responseRetentionHooks{beforeResponse: func(w http.ResponseWriter, _ *http.Request) bool {
							if calls.Add(1) != 1 {
								return false
							}
							w.Header().Set("Content-Type", "application/json")
							w.Header().Set("Retry-After", "60")
							w.WriteHeader(http.StatusTooManyRequests)
							_, _ = io.WriteString(w, `{"code":"resource-exhausted","error":"Too many requests for team f1692451-874f-4765-ab9b-5285f6c6ff65 and model grok-4.5. Requests per Minute (actual/limit): 2/2."}`)
							return true
						}})
						if action == "same_team_rotation" {
							credential, err := f.accounts.Get(context.Background(), f.accountID)
							if err != nil {
								t.Fatal(err)
							}
							credential.TeamID = "f1692451-874f-4765-ab9b-5285f6c6ff65"
							if _, _, err := f.accounts.UpsertByIdentity(context.Background(), credential); err != nil {
								t.Fatal(err)
							}
						}
						server := httptest.NewServer(f.router)
						defer server.Close()
						client := server.Client()
						client.Timeout = 5 * time.Second
						send := func() int {
							payload := map[string]any{"model": f.model, "input": "hello", "store": false, "stream": stream}
							if operation != "responses" {
								delete(payload, "input")
								payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
							}
							if operation == "messages" {
								payload["max_tokens"] = 16
							}
							body, err := json.Marshal(payload)
							if err != nil {
								t.Fatal(err)
							}
							req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, strings.NewReader(string(body)))
							if err != nil {
								t.Fatal(err)
							}
							req.Header.Set("Content-Type", "application/json")
							req.Header.Set("Authorization", "Bearer "+f.secret)
							req.Header.Set("Anthropic-Version", "2023-06-01")
							resp, err := client.Do(req)
							if err != nil {
								t.Fatal(err)
							}
							_, _ = io.Copy(io.Discard, resp.Body)
							_ = resp.Body.Close()
							return resp.StatusCode
						}
						if status := send(); status != 429 || calls.Load() != 1 {
							t.Fatalf("initial limit: status=%d calls=%d", status, calls.Load())
						}
						if action != "original" {
							credential, err := f.accounts.Get(context.Background(), f.accountID)
							if err != nil {
								t.Fatal(err)
							}
							oldGeneration := credential.CredentialGeneration
							cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
							if err != nil {
								t.Fatal(err)
							}
							credential.EncryptedAccessToken, err = cipher.Encrypt("replacement-material")
							if err != nil {
								t.Fatal(err)
							}
							next, _, err := f.accounts.UpsertByIdentity(context.Background(), credential)
							if err != nil || next.CredentialGeneration <= oldGeneration {
								t.Fatalf("replacement generation=%d old=%d err=%v", next.CredentialGeneration, oldGeneration, err)
							}
						}
						status := send()
						wantStatus, wantCalls := 429, int32(1)
						if action == "replacement" {
							wantStatus, wantCalls = 200, 2
						}
						if action == "replacement" && status == 200 {
							record := f.lastAudit(t)
							if record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" {
								t.Errorf("replacement completion=%+v", record)
							}
						}
						if status != wantStatus || calls.Load() != wantCalls {
							t.Errorf("next request status=%d want=%d upstream_calls=%d want=%d", status, wantStatus, calls.Load(), wantCalls)
						}
					})
				}
			}
		}
	}
}
