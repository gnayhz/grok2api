package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type executionPanicConsoleAdapter struct {
	*console.Adapter
	entered atomic.Bool
}

func (a *executionPanicConsoleAdapter) GenerateImage(context.Context, provider.ImageGenerationRequest) (*provider.Response, error) {
	a.entered.Store(true)
	panic("injected image preparation failure")
}

func (a *executionPanicConsoleAdapter) SynthesizeSpeech(context.Context, provider.TTSRequest) (provider.TTSResult, error) {
	a.entered.Store(true)
	panic("injected voice preparation failure")
}

func (a *executionPanicConsoleAdapter) DialVoiceWebSocket(context.Context, provider.VoiceWebSocketRequest) (provider.VoiceWebSocketConn, func(), error) {
	a.entered.Store(true)
	panic("injected websocket preparation failure")
}

func TestHTTPMediaPanicReleasesUnhandedAccountLease(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, runtime := range []string{"memory", "redis"} {
			for _, operation := range []struct{ path, model, payload string }{
				{"images/generations", "grok-imagine-image", `{"prompt":"hello"}`},
				{"audio/speech", "grok-voice-latest", `{"input":"hello","voice":"eve"}`},
				{"realtime", "grok-voice-latest", `{}`},
			} {
				t.Run(dialect+"/"+runtime+"/"+operation.path, func(t *testing.T) {
					limiter := httpSelectorCancellationLimiter(t, runtime)
					var adapter *executionPanicConsoleAdapter
					f := newProviderCompletionFixtureOnDatabase(t, "https://unused.invalid", operation.model, account.ProviderConsole, nil, func(base provider.Adapter) provider.Adapter {
						adapter = &executionPanicConsoleAdapter{Adapter: base.(*console.Adapter)}
						return adapter
					}, nil, nil, compactionDatabase(t, dialect), "", limiter)
					router := gin.New()
					router.Use(middleware.RequestID(nil), gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) { c.AbortWithStatus(500) }), middleware.ClientAuth(f.clientService))
					NewHandler(f.service, nil, 1<<20).Register(router.Group("/v1"))
					done := make(chan struct{})
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						ctx, cancel := context.WithCancel(r.Context())
						defer func() { cancel(); close(done) }()
						router.ServeHTTP(w, r.WithContext(ctx))
					}))
					defer server.Close()
					payload := map[string]any{}
					_ = json.Unmarshal([]byte(operation.payload), &payload)
					payload["model"] = f.publicModel
					body, _ := json.Marshal(payload)
					method, path := http.MethodPost, "/v1/"+operation.path
					if operation.path == "realtime" {
						method, path = http.MethodGet, path+"?model="+f.publicModel
					}
					req, err := http.NewRequest(method, server.URL+path, strings.NewReader(string(body)))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+f.created.Secret)
					req.Header.Set("Content-Type", "application/json")
					if operation.path == "realtime" {
						req.Header.Set("Connection", "Upgrade")
						req.Header.Set("Upgrade", "websocket")
						req.Header.Set("Sec-WebSocket-Version", "13")
						req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
					}
					client := server.Client()
					client.Timeout = 5 * time.Second
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					raw, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Fatal("handler did not exit")
					}
					if !adapter.entered.Load() || resp.StatusCode != 500 {
						t.Fatalf("adapter entered=%t status=%d body=%s", adapter.entered.Load(), resp.StatusCode, raw)
					}
					active, err := limiter.Current(context.Background(), repository.AccountConcurrencyKey(f.account.ID))
					if err != nil || active != 0 {
						t.Errorf("finished media retained account capacity: active=%d err=%v", active, err)
					}
				})
			}
		}
	}
}

type executionProbeRefreshAdapter struct {
	*cli.Adapter
	fail func()
}

func (a *executionProbeRefreshAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	a.fail()
	return provider.RefreshedCredential{}, errors.New("injected credential preparation failure")
}

func TestQualityProbePreparationReleasesAllUnhandedCapacity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, runtime := range []string{"memory", "redis"} {
			for _, stage := range []string{"panic", "error"} {
				t.Run(dialect+"/"+runtime+"/"+stage, func(t *testing.T) {
					limiter := httpSelectorCancellationLimiter(t, runtime)
					var fail func()
					f := newResponseRetentionFixture(t, dialect, account.ProviderBuild, responseRetentionHooks{useModelService: true, limiter: limiter, wrapAdapter: func(base provider.Adapter) provider.Adapter {
						return &executionProbeRefreshAdapter{Adapter: base.(*cli.Adapter), fail: func() { fail() }}
					}})
					view, err := f.accounts.Get(context.Background(), f.accountID)
					if err != nil {
						t.Fatal(err)
					}
					credential := view
					credential.ExpiresAt = time.Now().Add(-time.Hour)
					credential.EncryptedRefreshToken = credential.EncryptedAccessToken
					if _, _, err := f.accounts.UpsertByIdentity(context.Background(), credential); err != nil {
						t.Fatal(err)
					}
					keys := []string{repository.AccountConcurrencyKey(f.accountID), fmt.Sprintf("quality:identity/account/%d", f.accountID), "quality:identity/group/91", "quality:measurements"}
					entered := false
					fail = func() {
						entered = true
						for _, key := range keys {
							count, err := limiter.Current(context.Background(), key)
							if err != nil || count != 1 {
								t.Errorf("preparation did not hold %s: count=%d err=%v", key, count, err)
							}
						}
						if stage == "panic" {
							panic("probe preparation")
						}
					}
					var recovered any
					func() {
						defer func() { recovered = recover() }()
						result := f.gateway.ProbeExitJury(qualitymodel.WithProbeIdentity(context.Background(), 91, true), f.accountID, 0)
						if result.Outcome != qualitymodel.MeasurementError || result.Failure != qualitymodel.ProbeFailureCredential {
							t.Errorf("failure classification=%+v", result)
						}
					}()
					if !entered || (stage == "panic") != (recovered != nil) {
						t.Fatalf("credential entered=%t recovered=%v", entered, recovered)
					}
					for _, key := range keys {
						count, err := limiter.Current(context.Background(), key)
						if err != nil || count != 0 {
							t.Errorf("finished preparation retained %s: count=%d err=%v", key, count, err)
						}
					}
				})
			}
		}
	}
}
