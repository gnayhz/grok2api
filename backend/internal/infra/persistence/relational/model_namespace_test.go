package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestModelGroupRenamePreservesEachRoute(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"catalog", "manual", "manual_concurrent"} {
			t.Run(dialect+"/"+mode, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ctx := context.Background()
				if _, _, err := NewAccountRepository(a).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderConsole, Name: "console", SourceKey: "console", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive}); err != nil {
					t.Fatal(err)
				}
				ra, rb := NewModelRepository(a), NewModelRepository(b)
				origin := model.OriginCatalog
				if mode != "catalog" {
					origin = model.OriginManual
				}
				definitions := []model.Route{
					{PublicID: "legacy", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Origin: origin, Enabled: true},
					{PublicID: "legacy", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImageEdit, Origin: origin, Enabled: true},
				}
				if mode == "catalog" {
					if err := ra.ReplaceProviderRoutes(ctx, account.ProviderConsole, definitions); err != nil {
						t.Fatal(err)
					}
				} else {
					var id uint64
					if err := a.db.Model(&accountModel{}).Where("provider = ?", account.ProviderConsole).Pluck("id", &id).Error; err != nil {
						t.Fatal(err)
					}
					for _, route := range definitions {
						if _, err := ra.Create(ctx, route, []uint64{id}); err != nil {
							t.Fatal(err)
						}
					}
				}
				before, err := rb.GetByPublicIDCandidates(ctx, "legacy")
				if err != nil || len(before) != 2 {
					t.Fatalf("original route set = %+v, %v", before, err)
				}
				if mode == "catalog" {
					for index := range definitions {
						definitions[index].PublicID = "current"
					}
					if err := ra.ReplaceProviderRoutes(ctx, account.ProviderConsole, definitions); err != nil {
						t.Fatal(err)
					}
				} else if mode == "manual_concurrent" {
					results := make(chan error, len(before))
					start := make(chan struct{})
					for index, route := range before {
						go func() {
							<-start
							name := fmt.Sprintf("current-%d", index)
							repo := ra
							if index%2 == 1 {
								repo = rb
							}
							_, err := repo.Patch(ctx, route.ID, model.RoutePatch{PublicID: &name})
							results <- err
						}()
					}
					close(start)
					for range before {
						if err := <-results; err != nil {
							t.Fatal(err)
						}
					}
				} else {
					for index, route := range before {
						name := fmt.Sprintf("current-%d", index)
						if _, err := ra.Patch(ctx, route.ID, model.RoutePatch{PublicID: &name}); err != nil {
							t.Fatal(err)
						}
						aliases, err := rb.GetByPublicIDCandidates(ctx, "legacy")
						if err != nil || len(aliases) != 2 {
							t.Errorf("after manual rename %d legacy route set = %+v, %v; want original two targets", index, aliases, err)
						}
					}
				}
				after, err := rb.GetByPublicIDCandidates(ctx, "legacy")
				if err != nil || len(after) != 2 {
					t.Fatalf("legacy capability set after %s rename = %+v, %v; want original image and image_edit targets", mode, after, err)
				}
				for index, route := range after {
					if route.ID != before[index].ID || route.Capability != before[index].Capability {
						t.Fatalf("changed alias identity: before %+v after %+v", before, after)
					}
				}
				enabled := false
				if _, err := ra.Patch(ctx, before[0].ID, model.RoutePatch{Enabled: &enabled}); err != nil {
					t.Fatal(err)
				}
				filtered, err := rb.GetByPublicIDCandidates(ctx, "legacy")
				if err != nil || len(filtered) != 1 || filtered[0].ID != before[1].ID {
					t.Fatalf("disabled historical member leaked: %+v %v", filtered, err)
				}
				if err := ra.Delete(ctx, before[1].ID); err != nil {
					t.Fatal(err)
				}
				if _, err := rb.GetByPublicIDCandidates(ctx, "legacy"); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("deleted/disabled alias stayed available: %v", err)
				}
				if exists, err := rb.HasEnabledRouteByPublicID(ctx, "legacy"); err != nil || exists {
					t.Fatalf("disabled alias stayed configured-enabled: %t %v", exists, err)
				}

			})
		}
	}
}

func TestModelAliasRestoreAndNamespaceOwnership(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			ra, rb := NewModelRepository(a), NewModelRepository(b)
			definitions := []model.Route{
				{PublicID: "legacy", Provider: account.ProviderConsole, UpstreamModel: "upstream", Capability: model.CapabilityImage, Origin: model.OriginCatalog, Enabled: true},
				{PublicID: "legacy", Provider: account.ProviderConsole, UpstreamModel: "upstream", Capability: model.CapabilityImageEdit, Origin: model.OriginCatalog, Enabled: true},
			}
			if err := ra.ReplaceProviderRoutes(ctx, account.ProviderConsole, definitions); err != nil {
				t.Fatal(err)
			}
			before, err := findModelRoutesByPublicID(a.db, "legacy")
			if err != nil || len(before) != 2 {
				t.Fatalf("initial %+v %v", before, err)
			}
			for index := range definitions {
				definitions[index].PublicID = "current"
			}
			if err := rb.ReplaceProviderRoutes(ctx, account.ProviderConsole, definitions); err != nil {
				t.Fatal(err)
			}
			if _, err := ra.Create(ctx, model.Route{PublicID: "legacy", Provider: account.ProviderConsole, UpstreamModel: "unrelated", Capability: model.CapabilityImage, Enabled: true}, nil); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("unrelated alias takeover: %v", err)
			}
			for index := range definitions {
				definitions[index].PublicID = "legacy"
			}
			if err := ra.ReplaceProviderRoutes(ctx, account.ProviderConsole, definitions); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"legacy", "current"} {
				after, err := findModelRoutesByPublicID(b.db, name)
				if err != nil || len(after) != 2 || after[0].Route.ID != before[0].Route.ID || after[1].Route.ID != before[1].Route.ID {
					t.Fatalf("restored %s %+v %v", name, after, err)
				}
			}
			// The whole name is reserved by all previous owners, including manual
			// members; catalog promotion must check every historical edge.
			manual, err := ra.Create(ctx, model.Route{PublicID: "legacy", Provider: account.ProviderConsole, UpstreamModel: "manual", Capability: model.CapabilityImage, Enabled: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			moved := "manual-current"
			if _, err := ra.Patch(ctx, manual.ID, model.RoutePatch{PublicID: &moved}); err != nil {
				t.Fatal(err)
			}
			for index := range definitions {
				definitions[index].PublicID = "current"
			}
			if err := rb.ReplaceProviderRoutes(ctx, account.ProviderConsole, definitions); err != nil {
				t.Fatal(err)
			}
			for index := range definitions {
				definitions[index].PublicID = "legacy"
			}
			if err := ra.ReplaceProviderRoutes(ctx, account.ProviderConsole, definitions); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("catalog took manual historical owner: %v", err)
			}
			var aliases int64
			if err := b.db.Model(&modelRouteAliasModel{}).Where("alias = ?", "Console/legacy").Count(&aliases).Error; err != nil || aliases != 3 {
				t.Fatalf("promotion conflict lost edges: %d %v", aliases, err)
			}
			if err := ra.Delete(ctx, manual.ID); err != nil {
				t.Fatal(err)
			}
			if err := rb.ReplaceProviderRoutes(ctx, account.ProviderConsole, definitions); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestModelNamespaceCreationWaitsForRename(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ra, rb := NewModelRepository(a), NewModelRepository(b)
			route, err := ra.Create(ctx, model.Route{PublicID: "legacy", Provider: account.ProviderBuild, UpstreamModel: "upstream", Capability: model.CapabilityResponses, Enabled: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			hook := "g07_hold_alias_insert"
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
			renamed := make(chan error, 1)
			go func() {
				name := "current"
				_, err := ra.Patch(ctx, route.ID, model.RoutePatch{PublicID: &name})
				renamed <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			lockStarted := make(chan struct{}, 1)
			if dialect == "postgres" {
				if err := b.db.Callback().Raw().Before("gorm:raw").Register("g07_observe_namespace_wait", func(tx *gorm.DB) {
					if strings.Contains(tx.Statement.SQL.String(), "pg_advisory_xact_lock") {
						select {
						case lockStarted <- struct{}{}:
						default:
						}
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer b.db.Callback().Raw().Remove("g07_observe_namespace_wait")
			}
			created := make(chan error, 1)
			go func() {
				_, err := rb.Create(ctx, model.Route{PublicID: "legacy", Provider: account.ProviderBuild, UpstreamModel: "unrelated", Capability: model.CapabilityResponses, Enabled: true}, nil)
				created <- err
			}()
			if dialect == "postgres" {
				select {
				case <-lockStarted:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case err := <-created:
				t.Fatalf("namespace creation escaped pending rename: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			unblock()
			if err := <-renamed; err != nil {
				t.Fatal(err)
			}
			if err := <-created; !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("alias name was not reserved: %v", err)
			}
			rows, err := findModelRoutesByPublicID(b.db, "legacy")
			if err != nil || len(rows) != 1 || rows[0].Route.ID != route.ID {
				t.Fatalf("ambiguous alias %+v %v", rows, err)
			}
		})
	}
}

func TestModelNamespaceCancellationAndProviderIndependence(t *testing.T) {
	a, b := settingsDatabasePair(t, "postgres")
	ctx := context.Background()
	holder := a.db.Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	defer holder.Rollback()
	if err := lockModelNamespaces(holder, account.ProviderBuild); err != nil {
		t.Fatal(err)
	}
	repo := NewModelRepository(b)
	// An unrelated Provider remains independently editable while Build is locked.
	otherCtx, otherCancel := context.WithTimeout(ctx, time.Second)
	defer otherCancel()
	if _, err := repo.Create(otherCtx, model.Route{PublicID: "other", Provider: account.ProviderWeb, UpstreamModel: "other", Capability: model.CapabilityChat, Enabled: true}, nil); err != nil {
		t.Fatalf("unrelated Provider blocked: %v", err)
	}
	canceled, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	build := model.Route{PublicID: "after-cancel", Provider: account.ProviderBuild, UpstreamModel: "upstream", Capability: model.CapabilityResponses, Enabled: true}
	if _, err := repo.Create(canceled, build, nil); err == nil || canceled.Err() == nil {
		t.Fatalf("waiting write was not canceled: %v, %v", err, canceled.Err())
	}
	if err := holder.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, build, nil); err != nil {
		t.Fatalf("canceled namespace holder leaked: %v", err)
	}
}
