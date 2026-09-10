package relational

import (
	"context"
	"strings"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestClientKeyDeletingLastGrantedModelDoesNotAuthorizeOtherModels(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, _ := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			models, keys := NewModelRepository(a), NewClientKeyRepository(a)
			allowed, err := models.Create(ctx, model.Route{PublicID: "allowed", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5", Capability: model.CapabilityResponses, Enabled: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			unrelated, err := models.Create(ctx, model.Route{PublicID: "unrelated", Provider: account.ProviderBuild, UpstreamModel: "grok-4.3", Capability: model.CapabilityResponses, Enabled: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			secret := security.FormatClientKey("deadbeefcafe", strings.Repeat("ab", 24))
			created, err := keys.Create(ctx, clientkey.Key{Name: "restricted", Prefix: "deadbeefcafe", SecretHash: security.HashToken(secret), EncryptedSecret: "fixture", Enabled: true, AllowedModels: []uint64{allowed.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if created.AllowsModel(unrelated.ID) {
				t.Fatal("fixture unexpectedly allowed unrelated route")
			}
			if err := models.Delete(ctx, allowed.ID); err != nil {
				t.Fatal(err)
			}
			service := clientkeyapp.NewService("fixture", keys, nil, nil, 0, 0, nil)
			defer service.Close(ctx)
			actual, release, err := service.Authenticate(ctx, secret)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if actual.AllowsModel(unrelated.ID) {
				t.Fatalf("deleting last grant expanded key %d to all models: allowed=%v unrelated=%d", actual.ID, actual.AllowedModels, unrelated.ID)
			}
		})
	}
}
