package relational

import (
	"context"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/tokenhash"
	"path/filepath"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func BenchmarkClientKeyAuthorizationRead(b *testing.B) {
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "key-bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	models, keys := NewModelRepository(db), NewClientKeyRepository(db)
	ids := make([]uint64, 0, 3)
	for i := 0; i < 3; i++ {
		route, err := models.Create(ctx, model.Route{PublicID: fmt.Sprintf("model-%d", i), Provider: account.ProviderBuild, UpstreamModel: fmt.Sprintf("upstream-%d", i), Capability: model.CapabilityResponses, Enabled: true}, nil)
		if err != nil {
			b.Fatal(err)
		}
		ids = append(ids, route.ID)
	}
	for i := 0; i < 100; i++ {
		prefix := fmt.Sprintf("bench%07d", i)
		limit := int64(0)
		if i > 0 {
			limit = 1000000
		}
		_, err := keys.Create(ctx, clientkey.Key{Name: prefix, Prefix: prefix, SecretHash: tokenhash.HashToken(clientkey.FormatClientKey(prefix, "synthetic")), EncryptedSecret: "fixture", Enabled: true, BillingLimitUSDTicks: limit, AllowedModels: ids})
		if err != nil {
			b.Fatal(err)
		}
	}
	service := clientkeyapp.NewService("bench", keys, nil, nil, 0, 0, nil, security.RandomTokenSource{})
	b.Cleanup(func() { _ = service.Close(ctx) })
	for _, kind := range []string{"repository", "cached_auth", "finite_auth", "list_100"} {
		b.Run(kind, func(b *testing.B) {
			raw := clientkey.FormatClientKey("bench0000000", "synthetic")
			if kind == "finite_auth" {
				raw = clientkey.FormatClientKey("bench0000001", "synthetic")
			}
			if kind == "cached_auth" {
				_, release, err := service.Authenticate(ctx, raw)
				if err != nil {
					b.Fatal(err)
				}
				release()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				switch kind {
				case "repository":
					if _, err := keys.GetByPrefix(ctx, "bench0000000"); err != nil {
						b.Fatal(err)
					}
				case "cached_auth", "finite_auth":
					_, release, err := service.Authenticate(ctx, raw)
					if err != nil {
						b.Fatal(err)
					}
					release()
				case "list_100":
					if _, _, err := keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 100}}); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
