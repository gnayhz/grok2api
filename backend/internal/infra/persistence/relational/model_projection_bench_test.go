package relational

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

// Compare the public lookup and management projections over the same real
// catalog plus 1000 remotely named Build text routes; no unsupported fixtures.
func BenchmarkModelCapabilityProjection(b *testing.B) {
	ctx := context.Background()
	db, err := OpenSQLite(ctx, b.TempDir()+"/projection.db")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err = db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	repo, accounts := NewModelRepository(db), NewAccountRepository(db)
	for _, kind := range account.Providers() {
		var names []string
		if kind == account.ProviderBuild {
			for i := range 1000 {
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
	cases := []struct {
		name string
		run  func() error
	}{
		{"candidates", func() error { _, err := repo.GetByPublicIDCandidates(ctx, "grok-bench-0999"); return err }},
		{"enabled", func() error { _, err := repo.ListEnabled(ctx); return err }},
		{"active_page", func() error {
			_, _, err := repo.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20}, Filter: repository.ModelListFilter{ActiveScope: true}})
			return err
		}},
		{"support_page", func() error {
			_, _, err := repo.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20, Sort: repository.SortQuery{Field: "accountSupport", Direction: repository.SortDescending}}})
			return err
		}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := tc.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
