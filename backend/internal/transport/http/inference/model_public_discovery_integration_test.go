package inference

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestModelListingDoesNotReinventOccupiedAliases(t *testing.T) {
	for _, state := range []string{"disabled", "key_denied", "unsupported"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("occupied name reached upstream")
				w.WriteHeader(500)
			}))
			defer upstream.Close()
			fx := newProviderCompletionFixture(t, upstream.URL, "grok-4.5", account.ProviderBuild, nil, nil)
			base, err := fx.models.GetByPublicID(ctx, "grok-4.5")
			if err != nil {
				t.Fatal(err)
			}
			cap := model.CapabilityResponses
			if state == "unsupported" {
				cap = model.CapabilityVideo
			}
			_, err = fx.models.Create(ctx, model.Route{Provider: account.ProviderBuild, PublicID: "grok-4.5-low", UpstreamModel: "another-product", Capability: cap, Enabled: state != "disabled"}, []uint64{fx.account.ID})
			if err != nil {
				t.Fatal(err)
			}
			key := fx.created.Key
			key.AllowModelAliases = true
			if state == "key_denied" {
				key.AllowedModels = []uint64{base.ID}
			}
			if _, err = fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
				t.Fatal(err)
			}

			response := httptest.NewRecorder()
			listingRequest := httptest.NewRequest("GET", "/v1/models", nil)
			listingRequest.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			fx.router.ServeHTTP(response, listingRequest)
			if response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			if strings.Contains(response.Body.String(), `"id":"grok-4.5-low"`) {
				t.Errorf("discovery reinvented occupied %s alias: %s", state, response.Body.String())
			}
			request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"grok-4.5-low","input":"synthetic"}`))
			request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			request.Header.Set("Content-Type", "application/json")
			result := httptest.NewRecorder()
			fx.router.ServeHTTP(result, request)
			want := map[string]int{"disabled": 404, "key_denied": 403, "unsupported": 503}[state]
			if result.Code != want {
				t.Fatalf("occupied inference status %d want %d: %s", result.Code, want, result.Body.String())
			}
		})
	}
}
