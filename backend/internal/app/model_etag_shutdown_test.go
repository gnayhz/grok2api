package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/inference"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

type etagClosingModelRepository struct {
	repository.ModelRepository
	application *Application
	finished    chan error
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (r *etagClosingModelRepository) CompleteAccountCapabilitySync(ctx context.Context, ref modeldomain.CapabilitySyncRef, result modeldomain.CapabilitySyncResult) error {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	err := r.ModelRepository.CompleteAccountCapabilitySync(ctx, ref, result)
	observed := err
	if r.application.database.Stats().OpenConnections == 0 {
		observed = errors.Join(err, errors.New("SQL closed before ETag model refresh finalized"))
	} else if errors.Is(err, context.Canceled) {
		// Canceling a successful observation causes M05's existing bounded
		// failure write. SQL must stay available through that second write.
		observed = nil
	}
	r.finished <- observed
	return err
}

func TestETagRefreshFinishesBeforeApplicationClosesSQL(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, run := range []bool{false, true} {
			for _, operation := range []string{"responses", "chat/completions", "messages"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/run=%t/%s/stream=%t", dialect, run, operation, stream), func(t *testing.T) {
						ctx := context.Background()
						dsn := ""
						if dialect == "postgres" {
							dsn = os.Getenv("TEST_POSTGRES_DSN")
							if dsn == "" {
								t.Skip("requires isolated TEST_POSTGRES_DSN")
							}
							admin, err := pgx.Connect(ctx, dsn)
							if err != nil {
								t.Fatal(err)
							}
							t.Cleanup(func() { _ = admin.Close(ctx) })
							schema := fmt.Sprintf("etag_shutdown_%d", time.Now().UnixNano())
							if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
								t.Fatal(err)
							}
							t.Cleanup(func() {
								if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
									t.Error(err)
								}
							})
							parsed, err := url.Parse(dsn)
							if err != nil {
								t.Fatal(err)
							}
							query := parsed.Query()
							query.Set("search_path", schema)
							parsed.RawQuery = query.Encode()
							dsn = parsed.String()
						}
						origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							var payload map[string]any
							if r.Method == http.MethodPost {
								if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
									t.Error(err)
									http.Error(w, "bad fixture request", 400)
									return
								}
							}
							w.Header().Set("Content-Type", "application/json")
							switch r.URL.Path {
							case "/v1/models":
								w.Header().Set("ETag", "etag-current")
								_, _ = io.WriteString(w, `{"data":[{"id":"grok-4.5","object":"model"}]}`)
							case "/v1/responses":
								w.Header().Set("x-models-etag", "etag-current")
								if payload["stream"] == true {
									w.Header().Set("Content-Type", "text/event-stream")
									_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"etag-response\",\"status\":\"in_progress\"}}\n\n")
									_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"complete\"}\n\n")
									_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"etag-response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"complete\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
									return
								}
								_, _ = io.WriteString(w, `{"id":"etag-response","object":"response","status":"completed","model":"grok-4.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"complete"}]}],"usage":{"input_tokens":2,"output_tokens":1}}`)
							default:
								http.NotFound(w, r)
							}
						}))
						defer origin.Close()
						a := newLifecycleApplication(t, func(cfg *config.Config) {
							cfg.Database.Driver, cfg.Database.Postgres.DSN = dialect, dsn
							cfg.Provider.Build.BaseURL = origin.URL + "/v1"
							cfg.Provider.Build.FallbackBaseURL = "disabled"
							cfg.RequestRetry.Enabled = false
						})
						cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
						if err != nil {
							t.Fatal(err)
						}
						token, err := cipher.Encrypt("synthetic-etag-credential")
						if err != nil {
							t.Fatal(err)
						}
						credential, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "etag", SourceKey: "etag", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive, Enabled: true, MaxConcurrent: 2})
						if err != nil {
							t.Fatal(err)
						}
						if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-4.5"}); err != nil {
							t.Fatal(err)
						}
						if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, credential.ID, []string{"grok-4.5"}, time.Now().UTC()); err != nil {
							t.Fatal(err)
						}
						finished := make(chan error, 4)
						entered, release := make(chan struct{}), make(chan struct{})
						var once sync.Once
						defer once.Do(func() { close(release) })
						// Retain production M05/M07/Provider/SQL and HTTP handlers. The
						// repository gate holds the real final write so Close order is
						// deterministic rather than depending on network timing.
						a.models = modelapp.NewService(&etagClosingModelRepository{ModelRepository: a.modelRepo, application: a, finished: finished, entered: entered, release: release}, a.accountRepo, a.accounts, a.providers)
						a.models.SetLogger(a.logger)
						selector := gateway.NewSelector(a.accountRepo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), a.providers, time.Hour, time.Second, time.Minute)
						a.gateway = gateway.NewService(a.models, a.audits, a.accounts, a.clientKeys, a.providers, selector, relational.NewResponseRepository(a.database), 1)
						a.gateway.SetLogger(a.logger)
						router := gin.New()
						router.Use(middleware.RequestID(), middleware.ClientAuth(a.clientKeys))
						inference.NewHandler(a.gateway, nil, 1<<20).Register(router.Group("/v1"))
						a.server.Handler = router
						key, err := a.clientKeys.Create(ctx, clientkeyapp.CreateInput{Name: "etag", Enabled: true})
						if err != nil {
							t.Fatal(err)
						}
						var address string
						closeApplication := a.Close
						if run {
							cancel, runDone := startLifecycleApplication(t, a)
							probe := lifecycleGET(t, a)
							_ = probe.Body.Close()
							address = "http://" + a.server.Addr
							closeApplication = func() error { cancel(); return <-runDone }
						} else {
							if err := a.audits.Start(ctx); err != nil {
								t.Fatal(err)
							}
							server := httptest.NewServer(router)
							defer server.Close()
							address = server.URL
						}
						payload := map[string]any{"model": "grok-4.5", "input": "hello", "store": false, "stream": stream}
						if operation != "responses" {
							delete(payload, "input")
							payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
						}
						if operation == "messages" {
							payload["max_tokens"] = 16
						}
						encoded, err := json.Marshal(payload)
						if err != nil {
							t.Fatal(err)
						}
						request, err := http.NewRequest(http.MethodPost, address+"/v1/"+operation, strings.NewReader(string(encoded)))
						if err != nil {
							t.Fatal(err)
						}
						request.Header.Set("Anthropic-Version", "2023-06-01")
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Authorization", "Bearer "+key.Secret)
						client := &http.Client{Timeout: 5 * time.Second}
						response, err := client.Do(request)
						if err != nil {
							t.Fatal(err)
						}
						body, err := io.ReadAll(response.Body)
						_ = response.Body.Close()
						if err != nil || response.StatusCode != 200 {
							t.Fatalf("generation=%d body=%s err=%v", response.StatusCode, body, err)
						}
						select {
						case <-entered:
						case <-time.After(5 * time.Second):
							t.Fatal("ETag refresh did not reach the final model write")
						}
						closed := make(chan error, 1)
						go func() { closed <- closeApplication() }()
						returned := false
						select {
						case err := <-closed:
							returned = true
							t.Errorf("application returned before ETag final write: %v", err)
						case <-time.After(100 * time.Millisecond):
						}
						once.Do(func() { close(release) })
						select {
						case err := <-finished:
							if err != nil {
								t.Error(err)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("ETag refresh did not finish its write")
						}
						if !returned {
							select {
							case err := <-closed:
								if err != nil {
									t.Error(err)
								}
							case <-time.After(5 * time.Second):
								t.Fatal("application did not finish after ETag write")
							}
						}
						for len(finished) > 0 {
							if err := <-finished; err != nil {
								t.Error(err)
							}
						}
					})
				}
			}
		}
	}
}
