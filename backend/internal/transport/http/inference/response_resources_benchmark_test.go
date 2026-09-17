package inference

import (
	"context"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func BenchmarkResponseResourcePipeline(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderWeb} {
			for _, method := range []string{http.MethodGet, http.MethodDelete} {
				b.Run(dialect+"/"+string(kind)+"/"+method, func(b *testing.B) {
					ctx := context.Background()
					db := compactionDatabase(b, dialect)
					cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
					if err != nil {
						b.Fatal(err)
					}
					token, err := cipher.Encrypt("synthetic")
					if err != nil {
						b.Fatal(err)
					}
					accounts, models, audits, states := relational.NewAccountRepository(db), relational.NewModelRepository(db), relational.NewAuditRepository(db), relational.NewResponseRepository(db)
					model, authType := "grok-4.6", account.AuthTypeOAuth
					if kind == account.ProviderWeb {
						model, authType = "grok-chat-fast", account.AuthTypeSSO
					}
					credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: kind, AuthType: authType, Name: "cost", SourceKey: "cost", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, WebTier: account.WebTierBasic, MaxConcurrent: 2})
					if err != nil {
						b.Fatal(err)
					}
					if err = testsupport.Discover(ctx, models, kind, []string{model}); err != nil {
						b.Fatal(err)
					}
					if err = testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{model}, time.Now().UTC()); err != nil {
						b.Fatal(err)
					}
					route, err := models.GetByProviderUpstream(ctx, kind, model)
					if err != nil {
						b.Fatal(err)
					}
					key, err := relational.NewClientKeyRepository(db).Create(ctx, clientkey.Key{Name: "cost", Prefix: "cost", SecretHash: strings.Repeat("c", 64), EncryptedSecret: "synthetic", Enabled: true, ModelScope: clientkey.ModelScopeAll, RPMLimit: 100000, MaxConcurrent: 8})
					if err != nil {
						b.Fatal(err)
					}
					var calls atomic.Int64
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", "application/json")
						if r.Method == http.MethodDelete {
							_, _ = io.WriteString(w, `{"id":"cost","deleted":true}`)
						} else {
							_, _ = io.WriteString(w, `{"id":"cost","status":"completed","output":[]}`)
						}
					}))
					b.Cleanup(upstream.Close)
					var adapter provider.Adapter
					if kind == account.ProviderBuild {
						adapter = cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
					} else {
						adapter = resourceBenchmarkWeb(cipher, states)
					}
					registry := providerimpl.NewRegistry(adapter)
					sticky, capacity := memory.NewStickyStore(), memory.NewConcurrencyLimiter()
					maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
					clients := clientkeyapp.NewService("cost", nil, nil, nil, 100000, 8, nil, security.RandomTokenSource{})
					selector := selector.NewSelector(accounts, capacity, sticky, registry, time.Hour, time.Second, time.Minute)
					service := gateway.NewService(models, audits, maintenance, clients, registry, selector, historyapp.NewResponseResources(states), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 2)
					seed := func() {
						now := time.Now().UTC()
						if err := states.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "cost", AccountID: credential.ID, ClientKeyID: key.ID, ModelRouteID: route.ID, Provider: kind, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
							b.Fatal(err)
						}
						if kind == account.ProviderWeb {
							if err := states.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "cost", AccountID: credential.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: `{"id":"cost","status":"completed","output":[]}`, Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
								b.Fatal(err)
							}
						}
					}
					seed()
					input := gateway.ResourceInput{ClientKey: key, ResponseID: "cost"}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if method == http.MethodDelete && i > 0 {
							b.StopTimer()
							seed()
							b.StartTimer()
						}
						var result *gateway.Result
						var err error
						if method == http.MethodDelete {
							result, err = service.DeleteResponse(ctx, input)
						} else {
							result, err = service.GetResponse(ctx, input)
						}
						if err != nil {
							b.Fatal(err)
						}
						if result == nil || result.StatusCode != 200 {
							b.Fatal("resource not successful")
						}
						if _, err = io.Copy(io.Discard, result.Body); err != nil {
							b.Fatal(err)
						}
						_ = result.Body.Close()
						result.Finalize(gateway.Usage{}, "", "")
					}
					b.StopTimer()
					want := int64(b.N)
					if kind == account.ProviderWeb {
						want = 0
					}
					if calls.Load() != want {
						b.Fatalf("upstream calls=%d want=%d", calls.Load(), want)
					}
					b.ReportMetric(float64(calls.Load())/float64(b.N), "calls/op")
				})
			}
		}
	}
}
