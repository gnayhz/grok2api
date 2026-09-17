package relational

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	settingshttp "github.com/chenyme/grok2api/backend/internal/transport/http/settings"
	"github.com/gin-gonic/gin"
)

func TestSettingsHTTPCommitDelayPreservesValidatedMilliseconds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			base := settingsFileBaseline(t)
			ctx := context.Background()
			var applied []time.Duration
			service := settingsServiceOn(t, a, base, nil, func(next config.Config) {
				applied = append(applied, next.Audit.CommitDelay.Value())
			})
			cipher, err := security.NewCipher(base.Secrets.CredentialEncryptionKey)
			if err != nil {
				t.Fatal(err)
			}
			peer := NewRuntimeSettingsRepository(b, cipher)
			router := gin.New()
			settingshttp.NewHandler(service).Register(router.Group("/admin"))
			var revision uint64
			wantDelay := base.Audit.CommitDelay.Value()
			for _, tc := range []struct {
				name string
				ms   int64
				code int
			}{
				// This exact JSON integer fits JavaScript's safe range but wraps
				// the old nanosecond multiplication to a legal 5.448384ms value.
				{"positive_wrap", 18446744073715, http.StatusBadRequest},
				{"negative", -1, http.StatusBadRequest},
				{"above_policy", 51, http.StatusBadRequest},
				{"first_unrepresentable", math.MaxInt64/int64(time.Millisecond) + 1, http.StatusBadRequest},
				{"max_integer", math.MaxInt64, http.StatusBadRequest},
				{"exact_wrap", 1<<58 + 5, http.StatusBadRequest},
				{"minimum", 1, http.StatusOK},
				{"maximum", 50, http.StatusOK},
				{"legacy_zero_preserves_current", 0, http.StatusOK},
				{"following_valid_update", 7, http.StatusOK},
			} {
				t.Run(tc.name, func(t *testing.T) {
					get := httptest.NewRecorder()
					router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
					if get.Code != http.StatusOK {
						t.Fatalf("GET settings status=%d", get.Code)
					}
					var payload struct {
						Data struct {
							Config map[string]any `json:"config"`
						} `json:"data"`
					}
					decoder := json.NewDecoder(get.Body)
					decoder.UseNumber()
					if err := decoder.Decode(&payload); err != nil {
						t.Fatal(err)
					}
					payload.Data.Config["audit"].(map[string]any)["commitDelayMS"] = tc.ms
					body, err := json.Marshal(map[string]any{"revision": strconv.FormatUint(revision, 10), "config": payload.Data.Config})
					if err != nil {
						t.Fatal(err)
					}
					calls := len(applied)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body)))
					if tc.code == http.StatusOK {
						revision++
						calls++
						if tc.ms > 0 {
							wantDelay = time.Duration(tc.ms) * time.Millisecond
						}
					}
					loaded, _, persistedRevision, err := loadSettingsConfig(ctx, base, peer)
					if err != nil {
						t.Fatal(err)
					}
					snapshot := service.Get()
					if response.Code != tc.code || snapshot.Revision != revision || persistedRevision != revision || loaded.Audit.CommitDelay.Value() != wantDelay || len(applied) != calls {
						t.Fatalf("input_ms=%d status=%d want=%d local_revision=%d durable_revision=%d want_revision=%d durable_delay=%s want_delay=%s apply_calls=%d want_calls=%d", tc.ms, response.Code, tc.code, snapshot.Revision, persistedRevision, revision, loaded.Audit.CommitDelay.Value(), wantDelay, len(applied), calls)
					}
					if snapshot.ApplyPending || snapshot.AppliedRevision != revision || snapshot.Config.Audit.CommitDelayMS != int(wantDelay/time.Millisecond) {
						t.Fatalf("unexpected local application state: revision=%d applied=%d pending=%v delay_ms=%d", snapshot.Revision, snapshot.AppliedRevision, snapshot.ApplyPending, snapshot.Config.Audit.CommitDelayMS)
					}
					if len(applied) > 0 && applied[len(applied)-1] != wantDelay {
						t.Fatalf("consumer received %s, want %s", applied[len(applied)-1], wantDelay)
					}
				})
				if t.Failed() {
					return
				}
			}
		})
	}
}
