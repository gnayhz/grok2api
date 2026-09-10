package inference

import (
	"context"
	"encoding/json"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestRenamedModelGroupKeepsImageEndpointsAndKeyScope(t *testing.T) {
	for _, allowed := range []model.Capability{model.CapabilityImage, model.CapabilityImageEdit} {
		t.Run(string(allowed), func(t *testing.T) {
			ctx := context.Background()
			var calls, generated atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				generated.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": completionImagePNG}}})
			})
			defer upstream.Close()
			fx := newVoiceCompletionFixture(t, upstream.URL, "grok-imagine-image")
			original, err := fx.models.GetByPublicIDCandidates(ctx, fx.publicModel)
			if err != nil || len(original) != 2 {
				t.Fatalf("original candidates %+v %v", original, err)
			}
			key := fx.created.Key
			for _, route := range original {
				if route.Capability == allowed {
					key.AllowedModels = []uint64{route.ID}
				}
			}
			if len(key.AllowedModels) != 1 {
				t.Fatal("missing capability")
			}
			if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
				t.Fatal(err)
			}
			var renamed []model.Route
			for _, route := range model.CatalogRoutes(account.ProviderConsole) {
				if route.UpstreamModel == "grok-imagine-image" {
					route.PublicID = "Console/current-image"
					renamed = append(renamed, route)
				}
			}
			if err := fx.models.ReplaceProviderRoutes(ctx, account.ProviderConsole, renamed); err != nil {
				t.Fatal(err)
			}
			for _, entry := range []struct {
				path       string
				capability model.Capability
			}{{"/v1/images/generations", model.CapabilityImage}, {"/v1/images/edits", model.CapabilityImageEdit}} {
				for _, publicName := range []string{fx.publicModel, "current-image"} {
					body, _ := json.Marshal(map[string]any{"model": publicName, "prompt": "synthetic", "response_format": "b64_json", "image": map[string]string{"url": "data:image/png;base64," + completionImagePNG}})
					req := httptest.NewRequest(http.MethodPost, entry.path, strings.NewReader(string(body)))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+fx.created.Secret)
					rec := httptest.NewRecorder()
					before := generated.Load()
					fx.router.ServeHTTP(rec, req)
					if entry.capability == allowed {
						if rec.Code != http.StatusOK || generated.Load() != before+1 {
							t.Fatalf("authorized %s %s: %d %s calls %d", publicName, entry.path, rec.Code, rec.Body.String(), generated.Load()-before)
						}
					} else if rec.Code != http.StatusForbidden || generated.Load() != before {
						t.Fatalf("alias bypassed key route permission: %s %s %d %s calls %d", publicName, entry.path, rec.Code, rec.Body.String(), generated.Load()-before)
					}
				}
			}
			if generated.Load() != 2 {
				t.Fatalf("physical image calls = %d", generated.Load())
			}
			stored, err := fx.clients.Get(ctx, key.ID)
			if err != nil || len(stored.AllowedModels) != 1 || stored.AllowedModels[0] != key.AllowedModels[0] {
				t.Fatalf("permissions changed across rename %+v %v", stored, err)
			}
		})
	}
}
