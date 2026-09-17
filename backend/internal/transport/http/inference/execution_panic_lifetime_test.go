package inference

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

type executionPanicBuildAdapter struct {
	*cli.Adapter
	stage      string
	bodyClosed chan struct{}
}

func (a *executionPanicBuildAdapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	if a.stage == "forward" {
		panic("injected before forwarding")
	}
	response, err := a.Adapter.ForwardResponse(ctx, request)
	if a.stage == "body" && response != nil {
		response.Body = &executionPanicBody{ReadCloser: response.Body, closed: a.bodyClosed}
	}
	return response, err
}

type executionPanicBody struct {
	io.ReadCloser
	closed chan struct{}
	once   sync.Once
}

func (b *executionPanicBody) Read([]byte) (int, error) { panic("injected while preparing delivery") }
func (b *executionPanicBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { close(b.closed) })
	return err
}

func TestHTTPRequestPanicReleasesUnhandedAccountLease(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, runtime := range []string{"memory", "redis"} {
			for _, operation := range []string{"responses", "chat/completions", "messages"} {
				for _, stage := range []string{"forward", "body", "ordinary"} {
					t.Run(dialect+"/"+runtime+"/"+operation+"/"+stage, func(t *testing.T) {
						closed := make(chan struct{})
						f := newResponseRetentionFixture(t, dialect, account.ProviderBuild, responseRetentionHooks{
							limiter:    httpSelectorCancellationLimiter(t, runtime),
							middleware: []gin.HandlerFunc{gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) { c.AbortWithStatus(500) })},
							wrapAdapter: func(adapter provider.Adapter) provider.Adapter {
								return &executionPanicBuildAdapter{Adapter: adapter.(*cli.Adapter), stage: stage, bodyClosed: closed}
							},
						})
						done := make(chan struct{})
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							ctx, cancel := context.WithCancel(r.Context())
							defer func() { cancel(); close(done) }()
							f.router.ServeHTTP(w, r.WithContext(ctx))
						}))
						defer server.Close()
						payload := map[string]any{"model": f.model, "input": "hello", "stream": false}
						if operation != "responses" {
							delete(payload, "input")
							payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
						}
						if operation == "messages" {
							payload["max_tokens"] = 16
						}
						body, _ := json.Marshal(payload)
						req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, strings.NewReader(string(body)))
						if err != nil {
							t.Fatal(err)
						}
						req.Header.Set("Authorization", "Bearer "+f.secret)
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Anthropic-Version", "2023-06-01")
						client := server.Client()
						client.Timeout = 5 * time.Second
						resp, err := client.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						select {
						case <-done:
						case <-time.After(time.Second):
							t.Fatal("HTTP handler did not exit")
						}
						want := 500
						if stage == "ordinary" {
							want = 200
						}
						if resp.StatusCode != want {
							t.Fatalf("response status=%d expected=%d", resp.StatusCode, want)
						}
						if stage == "body" {
							select {
							case <-closed:
							case <-time.After(time.Second):
								t.Error("owned response body was not closed")
							}
						}
						active, err := f.limiter.Current(context.Background(), repository.AccountConcurrencyKey(f.accountID))
						if err != nil || active != 0 {
							t.Errorf("finished request retained account capacity: active=%d err=%v", active, err)
						}
						if stage == "ordinary" {
							record := f.lastAudit(t)
							if record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" {
								t.Errorf("ordinary request changed: generation=%s delivery=%s ledger=%s", record.GenerationOutcome, record.DeliveryOutcome, record.LedgerOutcome)
							}
						}
					})
				}
			}
		}
	}
}
