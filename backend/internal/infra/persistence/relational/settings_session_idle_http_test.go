package relational

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
	settingshttp "github.com/chenyme/grok2api/backend/internal/transport/http/settings"
	"github.com/gin-gonic/gin"
)

func TestSessionIdleHTTPPersistsAndConverges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			base := settingsFileBaseline(t)
			var applied time.Duration
			service := settingsServiceOn(t, a, base, nil, func(next config.Config) { applied = next.Provider.Build.SessionIdleConnTimeout.Value() })
			peer := settingsServiceOn(t, b, base, nil, nil)
			router := gin.New()
			settingshttp.NewHandler(service).Register(router.Group("/admin"))
			get := httptest.NewRecorder()
			router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
			var payload struct {
				Data struct {
					Config   map[string]any `json:"config"`
					Revision string         `json:"revision"`
				} `json:"data"`
			}
			if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			build := payload.Data.Config["providerBuild"].(map[string]any)
			if build["sessionIdleConnTimeout"] != "5m" {
				t.Fatalf("default: %v", build["sessionIdleConnTimeout"])
			}
			build["sessionIdleConnTimeout"] = "8m"
			send := func(revision string, want int) {
				t.Helper()
				body, err := json.Marshal(map[string]any{"revision": revision, "config": payload.Data.Config})
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body)))
				if response.Code != want {
					t.Fatalf("status=%d want=%d body=%s", response.Code, want, response.Body.String())
				}
			}
			send(payload.Data.Revision, http.StatusOK)
			if applied != 8*time.Minute || service.Get().ApplyPending || len(service.Get().RestartRequired) != 0 {
				t.Fatal("not hot applied")
			}
			if err := peer.ReloadPersisted(context.Background()); err != nil {
				t.Fatal(err)
			}
			if peer.Get().Config.ProviderBuild.SessionIdleConnTimeout != "8m" {
				t.Fatal("second connection did not converge")
			}
			send(payload.Data.Revision, http.StatusConflict)
			if got := settingsServiceOn(t, b, base, nil, nil).Get().Config.ProviderBuild.SessionIdleConnTimeout; got != "8m" {
				t.Fatalf("restart lost %q", got)
			}
			reset, err := service.ResetToDefaults(context.Background(), service.Get().Revision)
			if err != nil || reset.Config.ProviderBuild.SessionIdleConnTimeout != "5m" || applied != 5*time.Minute {
				t.Fatalf("reset: %v", err)
			}
		})
	}
}
