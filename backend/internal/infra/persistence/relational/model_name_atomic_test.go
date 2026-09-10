package relational

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"gorm.io/gorm"
)

func TestCatalogNameManualClaimIsAtomic(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			ra, rb := NewModelRepository(a), NewModelRepository(b)
			const retired = "Web/grok-imagine-image-quality-lite"
			old := model.Route{Provider: account.ProviderWeb, PublicID: retired, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true}
			if err := ra.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{old}); err != nil {
				t.Fatal(err)
			}
			var route modelRouteModel
			if err := a.db.First(&route).Error; err != nil {
				t.Fatal(err)
			}
			when := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			if err := a.db.Create(&modelRouteAliasModel{Alias: retired, ModelRouteID: route.ID, NameSource: string(model.NameSourceGenerated), CreatedAt: when}).Error; err != nil {
				t.Fatal(err)
			}
			name := "custom"
			failure := errors.New("fail primary after alias claim")
			hook := "g13_fail_primary_name"
			if err := a.db.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "model_routes" {
					_ = tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, err := ra.Patch(ctx, route.ID, model.RoutePatch{PublicID: &name})
			_ = a.db.Callback().Update().Remove(hook)
			if !errors.Is(err, failure) {
				t.Fatalf("failure not injected: %v", err)
			}
			var alias modelRouteAliasModel
			if err := b.db.Where("alias = ? AND model_route_id = ?", retired, route.ID).First(&alias).Error; err != nil || alias.NameSource != string(model.NameSourceGenerated) || !alias.CreatedAt.Equal(when) {
				t.Fatalf("failed claim changed alias: %+v %v", alias, err)
			}
			if err := b.db.First(&route, route.ID).Error; err != nil || route.NameSource != string(model.NameSourceGenerated) || route.PublicID != retired {
				t.Fatalf("failed claim changed primary: %+v %v", route, err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			hook = "g13_hold_manual_alias_claim"
			if err := a.db.Callback().Create().Before("gorm:create").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "model_route_aliases" {
					return
				}
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					_ = tx.AddError(ctx.Err())
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer a.db.Callback().Create().Remove(hook)
			claimed := make(chan error, 1)
			go func() {
				_, err := ra.Patch(ctx, route.ID, model.RoutePatch{PublicID: &name})
				claimed <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			waiting := make(chan struct{}, 1)
			if dialect == "postgres" {
				if err := b.db.Callback().Raw().Before("gorm:raw").Register("g13_observe_catalog_lock", func(tx *gorm.DB) {
					if strings.Contains(tx.Statement.SQL.String(), "pg_advisory_xact_lock") {
						select {
						case waiting <- struct{}{}:
						default:
						}
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer b.db.Callback().Raw().Remove("g13_observe_catalog_lock")
			}
			published := make(chan error, 1)
			go func() {
				if dialect == "sqlite" {
					waiting <- struct{}{}
				}
				published <- rb.ReplaceProviderRoutes(ctx, account.ProviderWeb, model.CatalogRoutes(account.ProviderWeb))
			}()
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-published:
				t.Fatalf("catalog escaped active manual namespace claim: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			unblock()
			if err := <-claimed; err != nil {
				t.Fatal(err)
			}
			if err := <-published; err != nil {
				t.Fatal(err)
			}
			if err := b.db.Where("alias = ? AND model_route_id = ?", retired, route.ID).First(&alias).Error; err != nil || alias.NameSource != string(model.NameSourceManual) || !alias.CreatedAt.Equal(when) {
				t.Fatalf("publication erased or downgraded manual claim: %+v %v", alias, err)
			}
			got, err := rb.GetByPublicIDIncludingDisabled(ctx, retired)
			if err != nil || got.ID != route.ID {
				t.Fatalf("manual alias target changed: %+v %v", got, err)
			}
		})
	}
}
