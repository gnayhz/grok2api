package inference

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// This complete HTTP discovery path is kept source-identical for before/after
// measurements: 30 catalog routes plus 1000 remotely named Build text routes.
func BenchmarkPublicModelDiscovery(b *testing.B) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, b.TempDir()+"/discovery.db")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err = db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	repo, accounts := relational.NewModelRepository(db), relational.NewAccountRepository(db)
	for _, kind := range account.Providers() {
		var names []string
		if kind == account.ProviderBuild {
			names = []string{"grok-4.3", "grok-4.5", "grok-4.6", "grok-build-0.1"}
			for i := 4; i < 1000; i++ {
				names = append(names, fmt.Sprintf("grok-bench-%04d", i))
			}
			if err := testsupport.Discover(ctx, repo, kind, names); err != nil {
				b.Fatal(err)
			}
		} else {
			if err := testsupport.Routes(ctx, repo, model.CatalogRoutes(kind)); err != nil {
				b.Fatal(err)
			}
			for _, product := range model.CatalogModels(kind) {
				names = append(names, product.UpstreamModel)
			}
		}
		credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: kind, Name: string(kind), SourceKey: string(kind), EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive, WebTier: account.WebTierHeavy})
		if err != nil {
			b.Fatal(err)
		}
		if err := testsupport.Capabilities(ctx, repo, accounts, credential.ID, names, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
	registry := provider.NewRegistry(cli.NewAdapter(cli.Config{}, nil), web.NewAdapter(web.Config{}, nil, nil, nil, nil), console.NewAdapter(console.Config{}, nil, nil, nil))
	service := modelapp.NewService(repo, accounts, nil, registry)
	handler := NewHandler(nil, service, 1<<20)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.ClientKey, clientkeydomain.Key{AllowModelAliases: true})
		c.Next()
	})
	router.GET("/v1/models", handler.listModels)
	for _, tc := range []struct{ name, url string }{{"openai", "/v1/models"}, {"codex", "/v1/models?client_version=1"}} {
		b.Run(tc.name, func(b *testing.B) {
			request := httptest.NewRequest("GET", tc.url, nil)
			b.ReportAllocs()
			for b.Loop() {
				result := httptest.NewRecorder()
				router.ServeHTTP(result, request)
				if result.Code != 200 {
					b.Fatal(result.Code, result.Body.String())
				}
			}
		})
	}
}
