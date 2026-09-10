package relational

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestModelPublicationPreservesManagedIdentityAndManualIntent(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			a, b := settingsDatabasePair(t, dialect)
			models, peer := NewModelRepository(a), NewModelRepository(b)
			service := modelapp.NewService(models, nil, nil, nil)
			other := modelapp.NewService(peer, nil, nil, nil)
			if err := service.PublishCatalogs(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := models.ListConfiguredEnabled(ctx)
			if err != nil || len(before) != 30 {
				t.Fatalf("initial catalog: %d %v", len(before), err)
			}
			// Disable every capability, including image_edit and TTS. Repeated
			// discovery must not re-enable these or synthesize extra text routes.
			ids := make([]uint64, len(before))
			for i, route := range before {
				ids[i] = route.ID
			}
			if _, err := models.UpdateManyEnabled(ctx, ids, false); err != nil {
				t.Fatal(err)
			}
			manual, err := models.Create(ctx, model.Route{Provider: account.ProviderConsole,
				PublicID: "grok-imagine-image", UpstreamModel: "manual-target", Capability: model.CapabilityImage,
				Origin: model.OriginManual, Enabled: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Both instances publish the same catalog through the application
			// owner while a discovery batch uses the shared M05 defaults.
			var wg sync.WaitGroup
			errs := make(chan error, 3)
			for _, run := range []func() error{
				func() error { return service.PublishCatalogs(ctx) },
				func() error { return other.PublishCatalogs(ctx) },
				func() error {
					for _, provider := range []account.Provider{account.ProviderWeb, account.ProviderConsole} {
						var names []string
						for _, product := range model.CatalogModels(provider) {
							names = append(names, product.UpstreamModel)
						}
						if err := testsupport.Discover(ctx, peer, provider, names); err != nil {
							return err
						}
					}
					return nil
				},
			} {
				wg.Add(1)
				go func() { defer wg.Done(); errs <- run() }()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, previous := range before {
				current, err := models.Get(ctx, previous.ID)
				if err != nil || current.Enabled || current.PublicID != previous.PublicID || current.UpstreamModel != previous.UpstreamModel || current.Capability != previous.Capability || current.Origin != model.OriginCatalog {
					t.Fatalf("catalog identity/intent changed: before=%+v after=%+v error=%v", previous, current, err)
				}
			}
			current, err := models.Get(ctx, manual.ID)
			if err != nil || current.PublicID != manual.PublicID || current.UpstreamModel != manual.UpstreamModel || !current.Enabled || current.Origin != model.OriginManual {
				t.Fatalf("manual target changed: %+v %v", current, err)
			}
			var count int64
			if err := a.db.Model(&modelRouteModel{}).Count(&count).Error; err != nil || count != 31 {
				t.Fatalf("extra/removed routes: %d %v", count, err)
			}
		})
	}
}

func TestModelPublicationFailureAndRetry(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, _ := settingsDatabasePair(t, dialect)
			models := NewModelRepository(db)
			// An unrelated manual alias owns a desired Console catalog name.
			// Web may publish before this failure; Console must remain atomic.
			manual, err := models.Create(ctx, model.Route{Provider: account.ProviderConsole, PublicID: "grok-4.3",
				UpstreamModel: "manual-target", Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			newName := "Console/manual-name"
			if _, err := models.Patch(ctx, manual.ID, model.RoutePatch{PublicID: &newName}); err != nil {
				t.Fatal(err)
			}
			service := modelapp.NewService(models, nil, nil, nil)
			if err := service.PublishCatalogs(ctx); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("catalog conflict: %v", err)
			}
			var consoleCount, webCount int64
			db.db.Model(&modelRouteModel{}).Where("provider = ?", account.ProviderConsole).Count(&consoleCount)
			db.db.Model(&modelRouteModel{}).Where("provider = ?", account.ProviderWeb).Count(&webCount)
			if consoleCount != 1 || webCount != 9 {
				t.Fatalf("partial publication: web=%d console=%d", webCount, consoleCount)
			}
			if err := models.Delete(ctx, manual.ID); err != nil {
				t.Fatal(err)
			}
			if err := service.PublishCatalogs(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := models.ListConfiguredEnabled(ctx)
			if err != nil {
				t.Fatal(err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if err := service.PublishCatalogs(canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled publication: %v", err)
			}
			after, err := models.ListConfiguredEnabled(ctx)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("canceled publication changed catalog: %v", err)
			}
		})
	}
}

func TestDiscoveredPublicationUsesExplicitM05Decision(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, _ := settingsDatabasePair(t, dialect)
			models := NewModelRepository(db)
			// A model name that previously triggered SQL media policy now carries
			// an explicit publication decision. SQL must persist it as supplied.
			decision := model.Route{Provider: account.ProviderWeb, PublicID: "Web/explicit-product",
				UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImageEdit, Origin: model.OriginDiscovered, Enabled: false}
			if err := models.MergeRoutes(ctx, account.ProviderWeb, []model.Route{decision}); err != nil {
				t.Fatal(err)
			}
			got, err := models.GetByPublicIDIncludingDisabled(ctx, decision.PublicID)
			if err != nil || got.UpstreamModel != decision.UpstreamModel || got.Capability != decision.Capability || got.Enabled {
				t.Fatalf("decision replaced: %+v %v", got, err)
			}
			valid := decision
			valid.PublicID = "Web/rollback"
			invalid := valid
			invalid.Provider = account.ProviderConsole
			if err := models.MergeRoutes(ctx, account.ProviderWeb, []model.Route{valid, invalid}); err == nil {
				t.Fatal("cross-provider publication accepted")
			}
			if _, err := models.GetByPublicIDIncludingDisabled(ctx, valid.PublicID); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("invalid batch partially committed: %v", err)
			}
		})
	}
}
