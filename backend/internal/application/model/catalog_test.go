package model

import (
	"context"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"path/filepath"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestModelSyncPublishesSharedCatalogDefaults(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		name := "account"
		if bulk {
			name = "bulk"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "publication.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			token, err := cipher.Encrypt("catalog-fixture")
			if err != nil {
				t.Fatal(err)
			}
			accounts, models := relational.NewAccountRepository(db), relational.NewModelRepository(db)
			var credentials []account.Credential
			for _, kind := range account.Providers() {
				v, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: kind, Name: string(kind), SourceKey: string(kind), EncryptedAccessToken: token,
					AuthStatus: account.AuthStatusActive, ExpiresAt: time.Now().Add(time.Hour), WebTier: account.WebTierHeavy})
				if err != nil {
					t.Fatal(err)
				}
				credentials = append(credentials, v)
			}
			// Web and Console use their actual static adapters; Build supplies a
			// deterministic remote observation including an unknown text model.
			registry := providerimpl.NewRegistry(
				&modelCapabilityAdapter{models: map[uint64][]string{credentials[0].ID: {"future-text", "grok-imagine-video-1.5"}}},
				webprovider.NewAdapter(webprovider.Config{}, nil, cipher, nil, nil),
				consoleprovider.NewAdapter(consoleprovider.Config{}, nil, cipher, nil))
			as := accountapp.NewService(accounts, relational.NewAuditRepository(db), memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
			service := NewService(models, accounts, as, registry)
			t.Cleanup(func() { _ = service.Close(context.Background()) })
			if err := service.PublishCatalogs(ctx); err != nil {
				t.Fatal(err)
			}
			original, err := models.ListConfiguredEnabled(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if bulk {
				if _, err := service.SyncObserved(ctx, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, credential := range credentials {
					if _, err := service.SyncAccount(ctx, credential.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			after, err := models.ListConfiguredEnabled(ctx)
			if err != nil || len(after) != 32 {
				t.Fatalf("published routes: %d %v", len(after), err)
			}
			for _, previous := range original {
				current, err := models.Get(ctx, previous.ID)
				if err != nil || current.PublicID != previous.PublicID || current.UpstreamModel != previous.UpstreamModel || current.Capability != previous.Capability {
					t.Fatalf("static product changed: %+v %v", current, err)
				}
			}
			for name, capability := range map[string]modeldomain.Capability{"future-text": modeldomain.CapabilityResponses, "grok-imagine-video-1.5": modeldomain.CapabilityVideo} {
				route, err := models.GetByPublicIDIncludingDisabled(ctx, "Build/"+name)
				if err != nil || route.Capability != capability || route.Origin != modeldomain.OriginDiscovered {
					t.Fatalf("Build discovery: %+v %v", route, err)
				}
			}
		})
	}
}
