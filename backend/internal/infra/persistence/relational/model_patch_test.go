package relational

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestModelPatchConcurrentFieldsAndAliases(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			ra, rb := NewModelRepository(a), NewModelRepository(b)
			acct := capabilitySyncAccount(t, a)
			route, err := ra.Create(ctx, model.Route{PublicID: "original", Provider: account.ProviderBuild, UpstreamModel: "upstream", Capability: model.CapabilityResponses, Enabled: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			errs := make(chan error, 10)
			var wg sync.WaitGroup
			for index := range 10 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					name, enabled, ids := fmt.Sprintf("renamed-%d", index), false, []uint64{acct.ID}
					patch := model.RoutePatch{PublicID: &name}
					if index == 8 {
						patch = model.RoutePatch{Enabled: &enabled}
					}
					if index == 9 {
						patch = model.RoutePatch{AccountIDs: &ids}
					}
					repo := ra
					if index%2 == 1 {
						repo = rb
					}
					_, err := repo.Patch(ctx, route.ID, patch)
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := rb.Get(ctx, route.ID)
			if err != nil || got.Enabled || len(got.BoundAccountIDs) != 1 || got.BoundAccountIDs[0] != acct.ID {
				t.Fatalf("independent fields: %+v err %v", got, err)
			}
			// Every successful name remains either canonical or a compatibility alias.
			for index := -1; index < 8; index++ {
				name := "Build/original"
				if index >= 0 {
					name = fmt.Sprintf("Build/renamed-%d", index)
				}
				if name == got.PublicID {
					continue
				}
				var count int64
				if err := a.db.Table("model_route_aliases").Where("alias = ? AND model_route_id = ?", name, route.ID).Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("lost alias %s count %d err %v", name, count, err)
				}
			}
			empty := []uint64{}
			cleared, err := ra.Patch(ctx, route.ID, model.RoutePatch{AccountIDs: &empty})
			if err != nil || len(cleared.BoundAccountIDs) != 0 || cleared.Enabled || cleared.PublicID != got.PublicID {
				t.Fatalf("explicit clear: %+v err %v", cleared, err)
			}
		})
	}
}

func TestModelPatchFailureIsAtomic(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			ra, rb := NewModelRepository(a), NewModelRepository(b)
			acct := capabilitySyncAccount(t, a)
			route, err := ra.Create(ctx, model.Route{PublicID: "original", Provider: account.ProviderBuild, UpstreamModel: "upstream", Capability: model.CapabilityResponses, Enabled: true}, []uint64{acct.ID})
			if err != nil {
				t.Fatal(err)
			}
			var notifications atomic.Int32
			ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { notifications.Add(1) })
			name, enabled, invalid := "failed-rename", false, []uint64{acct.ID + 90000}
			_, err = ra.Patch(ctx, route.ID, model.RoutePatch{PublicID: &name, Enabled: &enabled, AccountIDs: &invalid})
			if err == nil {
				t.Fatal("invalid foreign key accepted")
			}
			got, err := rb.Get(ctx, route.ID)
			if err != nil || got.PublicID != route.PublicID || !got.Enabled || len(got.BoundAccountIDs) != 1 || got.BoundAccountIDs[0] != acct.ID {
				t.Fatalf("partial commit: %+v err %v", got, err)
			}
			var aliases int64
			if err := b.db.Table("model_route_aliases").Where("model_route_id = ?", route.ID).Count(&aliases).Error; err != nil || aliases != 0 {
				t.Fatalf("rolled-back alias retained: %d %v", aliases, err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := ra.Patch(canceled, route.ID, model.RoutePatch{Enabled: &enabled}); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation = %v", err)
			}
			if _, err := ra.Patch(ctx, route.ID+1, model.RoutePatch{}); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("missing = %v", err)
			}
			if _, err := ra.Patch(ctx, route.ID, model.RoutePatch{}); err != nil {
				t.Fatal(err)
			}
			if notifications.Load() != 0 {
				t.Fatal("failed or empty patch emitted invalidation")
			}
			ra.SetInvalidationObserver(func(ctx context.Context, _ repository.InvalidationEvent) {
				visible, err := rb.Get(ctx, route.ID)
				if err != nil || visible.Enabled {
					t.Errorf("notification before commit: %+v %v", visible, err)
				}
				notifications.Add(1)
			})
			if _, err := ra.Patch(ctx, route.ID, model.RoutePatch{Enabled: &enabled}); err != nil {
				t.Fatal(err)
			}
			if notifications.Load() != 1 {
				t.Fatal("missing committed invalidation")
			}
			if err := rb.Delete(ctx, route.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := ra.Patch(ctx, route.ID, model.RoutePatch{Enabled: &enabled}); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("deleted route resurrected: %v", err)
			}
		})
	}
}
