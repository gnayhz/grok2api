package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestHTTPRetryAfterPreservesLongAccountCooldown(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole} {
			for _, source := range []string{"header", "body"} {
				for _, seconds := range []int64{300, 18446744193} {
					t.Run(fmt.Sprintf("%s/%s/%s/%d", dialect, kind, source, seconds), func(t *testing.T) {
						var calls atomic.Int32
						f := newResponseRetentionFixture(t, dialect, kind, responseRetentionHooks{beforeResponse: func(w http.ResponseWriter, r *http.Request) bool {
							calls.Add(1)
							w.Header().Set("Content-Type", "text/plain")
							if source == "header" {
								w.Header().Set("Retry-After", fmt.Sprint(seconds))
							}
							w.WriteHeader(http.StatusTooManyRequests)
							text := "Requests per Minute (actual/limit): 2/1"
							if source == "body" {
								text += fmt.Sprintf(". Resets in: %ds", seconds)
							}
							_, _ = io.WriteString(w, text)
							return true
						}})
						got := f.create(t, false, nil, "cooldown")
						if got.Code < 400 || f.generated.Load() != 0 || calls.Load() != 1 {
							t.Fatalf("first status=%d calls=%d generated=%d body=%s", got.Code, calls.Load(), f.generated.Load(), got.Body.String())
						}
						current, err := f.accounts.Get(context.Background(), f.accountID)
						if err != nil {
							t.Fatal(err)
						}
						if current.CooldownUntil == nil {
							t.Fatalf("no durable health cooldown: %+v", current)
						}
						delay := time.Until(*current.CooldownUntil)
						minimum := 299 * time.Second
						if seconds > 100000 {
							minimum = 24 * time.Hour
						}
						if delay < minimum || current.AuthStatus != account.AuthStatusActive || !current.Enabled {
							t.Fatalf("delay=%v minimum=%v auth=%s enabled=%t", delay, minimum, current.AuthStatus, current.Enabled)
						}
						record := f.lastAudit(t)
						if record.InputTokens != 0 || record.OutputTokens != 0 || len(record.GenerationUsages) != 0 {
							t.Fatalf("rejected request generated usage: %+v", record)
						}

						again := f.create(t, false, nil, "cooldown-next")
						if again.Code < 400 || calls.Load() != 1 {
							t.Fatalf("cooldown ignored by next request: status=%d calls=%d body=%s", again.Code, calls.Load(), again.Body.String())
						}
						// Console's quota reconciliation excludes the account from the
						// current candidate snapshot. Reconstruct M04/M06 to prove the
						// persisted cooldown survives loss of all routing caches.
						for _, protocol := range []string{"responses", "chat/completions", "messages"} {
							for _, streaming := range []bool{false, true} {
								f.restartGateway()
								payload := map[string]any{"model": f.model, "stream": streaming}
								if protocol == "responses" {
									payload["input"] = "hello"
								} else {
									payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
									payload["max_tokens"] = 32
								}
								body, _ := json.Marshal(payload)
								again = f.request(http.MethodPost, "/v1/"+protocol, body, "cooldown-restarted")
								if again.Code != http.StatusTooManyRequests || calls.Load() != 1 {
									t.Fatalf("restart %s/stream=%t status=%d calls=%d body=%s", protocol, streaming, again.Code, calls.Load(), again.Body.String())
								}
								retrySeconds, parseErr := strconv.ParseInt(again.Header().Get("Retry-After"), 10, 64)
								if parseErr != nil || retrySeconds < int64(minimum/time.Second) {
									t.Fatalf("restart %s/stream=%t Retry-After=%q err=%v body=%s", protocol, streaming, again.Header().Get("Retry-After"), parseErr, again.Body.String())
								}
							}
						}

					})
				}
			}
		}
	}
}
