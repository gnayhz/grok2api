package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	modelhttp "github.com/chenyme/grok2api/backend/internal/transport/http/model"
	"github.com/gin-gonic/gin"
)

func renameModelThroughHTTP(t *testing.T, service *modelapp.Service, id uint64, name string) {
	t.Helper()
	router := gin.New()
	modelhttp.NewHandler(service).Register(router.Group("/admin"))
	body, _ := json.Marshal(map[string]string{"publicId": name})
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/admin/models/%d", id), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	router.ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("admin rename failed: %d %s", result.Code, result.Body.String())
	}
}

func TestHTTPManualRetiredNameKeepsTextAndKeyScope(t *testing.T) {
	ctx := context.Background()
	var generated atomic.Int32
	upstream := completionWebUpstream(t, &generated)
	defer upstream.Close()
	fx := newProviderCompletionFixture(t, upstream.URL, "grok-chat-fast", account.ProviderWeb, nil, nil)
	service := modelapp.NewService(fx.models, fx.accounts, nil, fx.registry)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	const retired = "grok-imagine-image-quality-lite"
	route, err := service.Create(ctx, modelapp.CreateInput{PublicID: retired, Provider: account.ProviderWeb, UpstreamModel: "grok-chat-fast", Capability: model.CapabilityChat, Enabled: true, AccountIDs: []uint64{fx.account.ID}})
	if err != nil {
		t.Fatal(err)
	}
	renameModelThroughHTTP(t, service, route.ID, "team-text")
	key := fx.created.Key
	key.AllowModelAliases = false // Persisted names retain the same route grant.
	key.AllowedModels = []uint64{route.ID}
	if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"before", "published", "denied"} {
		if phase == "published" {
			if err := service.PublishCatalogs(ctx); err != nil {
				t.Fatal(err)
			}
		}
		want := http.StatusOK
		if phase == "denied" {
			base, err := fx.models.GetByPublicID(ctx, "grok-chat-fast")
			if err != nil {
				t.Fatal(err)
			}
			key.AllowedModels = []uint64{base.ID}
			if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
				t.Fatal(err)
			}
			want = http.StatusForbidden
		}
		for _, operation := range []string{"responses", "chat/completions", "messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", phase, operation, stream), func(t *testing.T) {
					payload := map[string]any{"model": retired, "stream": stream, "store": false}
					if operation == "responses" {
						payload["input"] = "synthetic naming"
					} else {
						payload["messages"] = []map[string]string{{"role": "user", "content": "synthetic naming"}}
					}
					if operation == "messages" {
						payload["max_tokens"] = 32
					}
					body, _ := json.Marshal(payload)
					req := httptest.NewRequest(http.MethodPost, "/v1/"+operation, strings.NewReader(string(body)))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+fx.created.Secret)
					req.Header.Set("Anthropic-Version", "2023-06-01")
					result := httptest.NewRecorder()
					before := generated.Load()
					fx.router.ServeHTTP(result, req)
					if result.Code != want {
						t.Fatalf("status %d want %d: %s", result.Code, want, result.Body.String())
					}
					if want == http.StatusOK {
						if generated.Load() != before+1 || !strings.Contains(result.Body.String(), "completion answer") {
							t.Fatalf("expected one delivered generation: calls %d body %s", generated.Load()-before, result.Body.String())
						}
					} else if generated.Load() != before {
						t.Fatal("alias bypassed Key route permissions")
					}
				})
			}
		}
	}
}

func TestHTTPManagedRetiredNameKeepsImageAndKeyScope(t *testing.T) {
	for _, again := range []bool{false, true} {
		t.Run(fmt.Sprintf("rename-again=%t", again), func(t *testing.T) {
			ctx := context.Background()
			var generated atomic.Int32
			upstream := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if r.URL.Path != "/ws/imagine/listen" {
					w.WriteHeader(404)
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				for range 2 {
					var request any
					if err := conn.ReadJSON(&request); err != nil {
						t.Error(err)
						return
					}
				}
				generated.Add(1)
				_ = conn.WriteJSON(map[string]any{"type": "image", "id": "name-image", "blob": completionImagePNG, "percentage_complete": 100})
				_ = conn.WriteJSON(map[string]any{"type": "json", "id": "name-image", "current_status": "completed", "moderated": false})
			}))
			defer upstream.Close()
			fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-image-quality", account.ProviderWeb, func(store provider.ImageAssetStore) provider.ImageAssetStore { return store })
			service := modelapp.NewService(fx.models, fx.accounts, nil, fx.registry)
			t.Cleanup(func() { _ = service.Close(context.Background()) })
			route, err := fx.models.GetByPublicID(ctx, "grok-imagine-image")
			if err != nil {
				t.Fatal(err)
			}
			const retired = "grok-imagine-image-quality-lite"
			renameModelThroughHTTP(t, service, route.ID, retired)
			if again {
				renameModelThroughHTTP(t, service, route.ID, "team-image")
			}
			key := fx.created.Key
			key.AllowedModels = []uint64{route.ID}
			if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{"before", "published", "denied"} {
				if phase == "published" {
					if err := service.PublishCatalogs(ctx); err != nil {
						t.Fatal(err)
					}
				}
				want := http.StatusOK
				if phase == "denied" {
					other, err := fx.models.GetByPublicIDIncludingDisabled(ctx, "grok-imagine-image-lite")
					if err != nil {
						t.Fatal(err)
					}
					key.AllowedModels = []uint64{other.ID}
					if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
						t.Fatal(err)
					}
					want = http.StatusForbidden
				}
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t", phase, stream), func(t *testing.T) {
						body, _ := json.Marshal(map[string]any{"model": retired, "prompt": "synthetic naming", "response_format": "b64_json", "stream": stream})
						req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Authorization", "Bearer "+fx.created.Secret)
						result := httptest.NewRecorder()
						before := generated.Load()
						fx.router.ServeHTTP(result, req)
						if result.Code != want {
							t.Fatalf("status %d want %d: %s", result.Code, want, result.Body.String())
						}
						if want == http.StatusOK {
							if generated.Load() != before+1 || !strings.Contains(result.Body.String(), completionImagePNG) {
								t.Fatalf("expected one delivered image: calls %d body %s", generated.Load()-before, result.Body.String())
							}
						} else if generated.Load() != before {
							t.Fatal("image alias bypassed Key route permissions")
						}
					})
				}
			}
		})
	}
}
