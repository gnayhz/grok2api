package inference

import (
	"context"
	"encoding/json"
	"fmt"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type httpAuthReadGate struct {
	repository.ClientKeyRepository
	read, resume chan struct{}
	held         atomic.Bool
}

func (r *httpAuthReadGate) GetByPrefix(ctx context.Context, prefix string) (clientkey.Key, error) {
	value, err := r.ClientKeyRepository.GetByPrefix(ctx, prefix)
	if r.held.CompareAndSwap(false, true) {
		close(r.read)
		select {
		case <-r.resume:
		case <-ctx.Done():
			return clientkey.Key{}, ctx.Err()
		}
	}
	return value, err
}

func TestRevokedCacheCannotReauthorizeLaterHTTPRequests(t *testing.T) {
	for _, provider := range []account.Provider{account.ProviderBuild, account.ProviderWeb, account.ProviderConsole} {
		for _, mutation := range []string{"disable", "restrict_empty"} {
			t.Run(string(provider)+"/"+mutation, func(t *testing.T) {
				var upstreamCalls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { upstreamCalls.Add(1); w.WriteHeader(500) }))
				defer upstream.Close()
				upstreamModel := map[account.Provider]string{account.ProviderBuild: "grok-4.5", account.ProviderConsole: "grok-4.3", account.ProviderWeb: "grok-chat-fast"}[provider]
				fx := newProviderCompletionFixture(t, upstream.URL, upstreamModel, provider, nil, nil)
				zero, noBudget := 0, int64(0)
				if _, err := fx.clients.Patch(context.Background(), fx.created.Key.ID, clientkey.ManagementPatch{RPMLimit: &zero, MaxConcurrent: &zero, BillingLimitUSDTicks: &noBudget}); err != nil {
					t.Fatal(err)
				}
				held := &httpAuthReadGate{ClientKeyRepository: fx.clients, read: make(chan struct{}), resume: make(chan struct{})}
				resume := sync.OnceFunc(func() { close(held.resume) })
				defer resume()
				keys := clientkeyapp.NewService("test-owner", held, nil, nil, 0, 0, nil, security.RandomTokenSource{})
				t.Cleanup(func() { closeClientKeyService(t, keys) })
				models := modelapp.NewService(fx.models, fx.accounts, fx.accountService, fx.registry)
				t.Cleanup(func() { _ = models.Close(context.Background()) })
				router := gin.New()
				router.Use(middleware.RequestID(nil), middleware.ClientAuth(keys))
				NewHandler(fx.service, models, 1<<20).Register(router.Group("/v1"))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				oldRequest := httptest.NewRequest("GET", "/v1/models", nil).WithContext(ctx)
				oldRequest.Header.Set("Authorization", "Bearer "+fx.created.Secret)
				oldResponse := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { router.ServeHTTP(oldResponse, oldRequest); close(done) }()
				select {
				case <-held.read:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				wantedStatus := http.StatusUnauthorized
				var patch clientkeyapp.UpdateInput
				if mutation == "disable" {
					disabled := false
					patch.Enabled = &disabled
				} else {
					wantedStatus = http.StatusForbidden
					restricted := clientkey.ModelScopeRestricted
					patch.ModelScope = &restricted
				}
				if _, err := keys.Update(ctx, fx.created.Key.ID, patch); err != nil {
					t.Fatal(err)
				}
				resume()
				<-done
				if oldResponse.Code != 200 {
					t.Fatalf("earlier request could not complete its existing snapshot: %d", oldResponse.Code)
				}
				for _, protocol := range []string{"responses", "chat/completions", "messages"} {
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
							payload := map[string]any{"model": fx.publicModel, "stream": stream}
							if protocol == "responses" {
								payload["input"] = "synthetic"
							} else {
								payload["messages"] = []map[string]string{{"role": "user", "content": "synthetic"}}
								payload["max_tokens"] = 16
							}
							body, _ := json.Marshal(payload)
							request := httptest.NewRequest("POST", "/v1/"+protocol, strings.NewReader(string(body)))
							request.Header.Set("Content-Type", "application/json")
							request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
							request.Header.Set("anthropic-version", "2023-06-01")
							response := httptest.NewRecorder()
							router.ServeHTTP(response, request)
							if response.Code != wantedStatus || upstreamCalls.Load() != 0 {
								t.Fatalf("stale auth reached execution: status=%d, upstream=%d, body=%s", response.Code, upstreamCalls.Load(), response.Body.String())
							}
						})
					}
				}
			})
		}
	}
}
