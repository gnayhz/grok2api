package settings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func settingsHTTPFixture(t *testing.T, targets ...settingsapp.ApplyTarget) (*settingsapp.Service, *relational.RuntimeSettingsRepository, *gin.Engine) {
	t.Helper()
	ctx := context.Background()
	t.Setenv(config.DatabaseURLEnv, "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("secrets:\n  jwtSecret: '12345678901234567890123456789012'\n  credentialEncryptionKey: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base.Secrets.CredentialEncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewRuntimeSettingsRepository(db, cipher)
	service := settingsapp.NewService(base, time.Time{}, 0, repo, nil, targets)
	service.SetFileConfig(base)
	router := gin.New()
	NewHandler(service).Register(router.Group("/"))
	return service, repo, router
}

func TestResetRevisionHTTPContract(t *testing.T) {
	ctx := context.Background()
	service, repo, router := settingsHTTPFixture(t)
	for _, tc := range []struct {
		body     string
		code     int
		revision uint64
	}{
		{"", http.StatusOK, 1}, // older clients still perform a server-observed CAS
		{`{"revision":"0"}`, http.StatusConflict, 1},
		{`{"revision":"1"}`, http.StatusOK, 2},
		{`{"revision":2}`, http.StatusBadRequest, 2},
		{`{}`, http.StatusBadRequest, 2},
		{`null`, http.StatusBadRequest, 2},
		{`{"revision":"2"}`, http.StatusOK, 3},
	} {
		request := httptest.NewRequest(http.MethodDelete, "/settings", strings.NewReader(tc.body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tc.code || service.Get().Revision != tc.revision {
			t.Fatalf("body=%q code=%d revision=%d; want code=%d revision=%d", tc.body, response.Code, service.Get().Revision, tc.code, tc.revision)
		}
	}
	// Another writer advances SQL. The legacy empty DELETE must still conflict
	// when this service has not observed the newer durable version.
	if _, _, err := repo.Reset(ctx, 3); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/settings", nil))
	if response.Code != http.StatusConflict || service.Get().Revision != 3 {
		t.Fatalf("stale legacy reset status=%d", response.Code)
	}
}
