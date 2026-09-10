package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCatalogReplacedAliasRequiresExplicitRestoration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewModelRepository(a)
			catalog := []model.Route{{Provider: account.ProviderConsole, PublicID: "old-product", UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Enabled: true}}
			if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, catalog); err != nil {
				t.Fatal(err)
			}
			owner, err := repo.GetByPublicIDIncludingDisabled(ctx, "old-product")
			if err != nil {
				t.Fatal(err)
			}
			const replacement = "Console/replacement"
			when := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			alias := modelRouteAliasModel{Alias: replacement, ModelRouteID: owner.ID, NameSource: string(model.NameSourceLegacy), CreatedAt: when}
			if err := a.db.Create(&alias).Error; err != nil {
				t.Fatal(err)
			}
			catalog = append(catalog, model.Route{Provider: account.ProviderConsole, PublicID: replacement, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true})
			if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, catalog); err != nil {
				t.Fatal(err)
			}
			matches, err := findModelRoutesByPublicID(b.db, replacement)
			if err != nil || len(matches) != 1 || matches[0].Route.ID == owner.ID {
				t.Fatalf("replaced alias entered live lookup: %+v %v", matches, err)
			}
			snapshot, err := repo.ReadPublicSnapshot(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, edge := range snapshot.Aliases {
				if edge.Name == replacement && edge.RouteID == owner.ID {
					t.Fatal("replaced alias entered public discovery snapshot")
				}
			}
			// Removing the newer product must not resurrect a legacy protocol alias.
			if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, catalog[:1]); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.GetByPublicIDIncludingDisabled(ctx, replacement); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("replaced alias revived without a name claim: %v", err)
			}
			if exists, err := repo.HasEnabledRouteByPublicID(ctx, replacement); err != nil || exists {
				t.Fatalf("replaced alias reserved enabled name: %t %v", exists, err)
			}
			// Retained inactive facts do not reserve the name for an unrelated route.
			other, err := repo.Create(ctx, model.Route{Provider: account.ProviderConsole, PublicID: replacement, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true}, nil)
			if err != nil {
				t.Fatalf("inactive history blocked a new name claim: %v", err)
			}
			if err := repo.Delete(ctx, other.ID); err != nil {
				t.Fatal(err)
			}
			name := replacement
			if _, err := repo.Patch(ctx, owner.ID, model.RoutePatch{PublicID: &name}); err != nil {
				t.Fatal(err)
			}
			if err := b.db.Where("alias = ? AND model_route_id = ?", replacement, owner.ID).First(&alias).Error; err != nil || alias.ReplacedByCatalog || alias.NameSource != string(model.NameSourceManual) || !alias.CreatedAt.Equal(when) {
				t.Fatalf("explicit claim did not restore original relationship: %+v %v", alias, err)
			}
			name = "custom"
			if _, err := repo.Patch(ctx, owner.ID, model.RoutePatch{PublicID: &name}); err != nil {
				t.Fatal(err)
			}
			if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, catalog[:1]); err != nil {
				t.Fatal(err)
			}
			matches, err = findModelRoutesByPublicID(b.db, replacement)
			if err != nil || len(matches) != 1 || matches[0].Route.ID != owner.ID {
				t.Fatalf("restored manual alias lost after publication: %+v %v", matches, err)
			}
			// The database also rejects an impossible archived manual relationship.
			if err := a.db.Model(&modelRouteAliasModel{}).Where("alias = ? AND model_route_id = ?", replacement, owner.ID).Update("replaced_by_catalog", true).Error; err == nil {
				t.Fatal("manual relationship accepted catalog replacement state")
			}
		})
	}
}
