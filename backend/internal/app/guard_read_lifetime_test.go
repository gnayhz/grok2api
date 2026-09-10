package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

// A management read has its own lifetime. Ending it must not disable the
// already installed policy used by independent, authenticated HTTP requests.
func TestApplicationGuardReadLifetimeDoesNotPoisonInference(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			dsn := os.Getenv("TEST_POSTGRES_DSN")
			if dialect == "postgres" && dsn == "" {
				t.Skip("requires isolated TEST_POSTGRES_DSN")
			}
			var calls atomic.Int32
			var degraded atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/responses" {
					http.Error(w, "unused fixture endpoint", 404)
					return
				}
				n := calls.Add(1)
				var request struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				id := fmt.Sprintf("guard-read-%d", n)
				thinking := !degraded.Load()
				output := []any{}
				if thinking {
					output = append(output, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "calculate"}}})
				}
				output = append(output, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "The verified answer is 64."}}})
				result := map[string]any{"id": id, "object": "response", "status": "completed", "model": "grok-4.6", "output": output, "usage": map[string]any{"input_tokens": 20, "output_tokens": 64}}
				if !request.Stream {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(result)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"status\":\"in_progress\"}}\n\n", id)
				if thinking {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"calculate\"}\n\n")
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"The verified answer is 64.\"}\n\n")
				raw, _ := json.Marshal(map[string]any{"type": "response.completed", "response": result})
				_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
			}))
			defer upstream.Close()
			a := newLifecycleApplication(t, func(cfg *config.Config) {
				cfg.Provider.Build.BaseURL = upstream.URL + "/v1"
				cfg.Provider.Build.FallbackBaseURL = "disabled"
				cfg.RequestRetry.Enabled = true
				cfg.RequestRetry.MaxAttempts = 1
				cfg.Routing.MaxAttempts = 1
				if dialect == "postgres" {
					cfg.Database.Driver = dialect
					cfg.Database.Postgres.DSN = dsn
				}
			})
			ctx := context.Background()
			installed, err := a.qualityGuard.Update(ctx, a.qualityGuard.Config())
			if err != nil {
				t.Fatal(err)
			}
			docs := relational.NewSettingsDocumentRepository(a.database, qualityguard.SettingsKey)
			durable, err := docs.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			token, err := cipher.Encrypt("synthetic-guard-read-token")
			if err != nil {
				t.Fatal(err)
			}
			credential, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "guard-read", SourceKey: "guard-read", Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), MaxConcurrent: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
				t.Fatal(err)
			}
			if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, credential.ID, []string{"grok-4.6"}, time.Now()); err != nil {
				t.Fatal(err)
			}
			key, err := a.clientKeys.Create(ctx, clientkeyapp.CreateInput{Name: "guard-read", Enabled: true, BillingLimitUSDTicks: 100000000000})
			if err != nil {
				t.Fatal(err)
			}
			a.startup.setPhase("running")
			server := httptest.NewServer(a.server.Handler)
			defer server.Close()
			request := func(operation string, streaming bool, wantStatus int) {
				t.Helper()
				body := map[string]any{"model": "grok-4.6", "stream": streaming, "store": false}
				if operation == "responses" {
					body["input"] = "hello"
					body["reasoning"] = map[string]any{"effort": "high"}
				} else {
					body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
				}
				if operation == "messages" {
					body["max_tokens"] = 2048
					body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
				}
				if operation == "chat/completions" {
					body["reasoning_effort"] = "high"
				}
				raw, _ := json.Marshal(body)
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, strings.NewReader(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+key.Secret)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("anthropic-version", "2023-06-01")
				before := calls.Load()
				response, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				output, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != wantStatus || calls.Load() != before+1 {
					t.Fatalf("%s stream=%v: status=%d calls=%d→%d body=%s", operation, streaming, response.StatusCode, before, calls.Load(), output)
				}
				if wantStatus == http.StatusOK && !strings.Contains(string(output), "verified answer") {
					t.Fatalf("response lost output: %s", output)
				}
				if wantStatus != http.StatusOK && !strings.Contains(string(output), "upstream_degraded") {
					t.Fatalf("degraded response not guarded: %s", output)
				}
			}
			for _, kind := range []string{"cancel", "deadline"} {
				readCtx, cancel := context.WithCancel(ctx)
				if kind == "deadline" {
					cancel()
					readCtx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				} else {
					cancel()
				}
				_, readErr := a.qualityGuard.Read(readCtx)
				cancel()
				expected := context.Canceled
				if kind == "deadline" {
					expected = context.DeadlineExceeded
				}
				if !errors.Is(readErr, expected) {
					t.Fatalf("%s read error=%v", kind, readErr)
				}
				// Exercise the actual production snapshot source and subsequent request
				// path before a healthy management read could repair poisoned readiness.
				for _, operation := range []string{"responses", "chat/completions", "messages"} {
					for _, streaming := range []bool{false, true} {
						request(operation, streaming, http.StatusOK)
					}
				}
				snapshot, snapshotErr := a.qualityGuard.Snapshot()
				if snapshotErr != nil || !reflect.DeepEqual(snapshot, installed) {
					t.Fatalf("installed snapshot changed: %+v %v", snapshot, snapshotErr)
				}
			}
			degraded.Store(true)
			request("responses", false, http.StatusServiceUnavailable)
			after, err := docs.Load(ctx)
			if err != nil || !reflect.DeepEqual(after, durable) {
				t.Fatalf("read changed persistent policy: %+v %v", after, err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
