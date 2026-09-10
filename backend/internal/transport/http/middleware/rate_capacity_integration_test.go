package middleware_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adminapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	adminhttp "github.com/chenyme/grok2api/backend/internal/transport/http/adminauth"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

func rateCapacityDatabase(t *testing.T, driver string) *relational.Database {
	t.Helper()
	ctx := context.Background()
	var db *relational.Database
	var err error
	if driver == "sqlite" {
		db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("isolated TEST_POSTGRES_DSN required")
		}
		conn, e := pgx.Connect(ctx, dsn)
		if e != nil {
			t.Fatal(e)
		}
		schema := fmt.Sprintf("rate_capacity_%d", time.Now().UnixNano())
		if _, e = conn.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
			conn.Close(ctx)
			t.Fatal(e)
		}
		t.Cleanup(func() {
			_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = conn.Close(context.Background())
		})
		parsed, e := url.Parse(dsn)
		if e == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
			q := parsed.Query()
			q.Set("search_path", schema)
			parsed.RawQuery = q.Encode()
			dsn = parsed.String()
		} else {
			dsn += " search_path=" + schema
		}
		db, err = relational.OpenPostgres(ctx, dsn, 4, 2)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAuthenticationPreservesLimitsUnderRuntimePressure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			db := rateCapacityDatabase(t, driver)
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			limiter := memory.NewRateLimiter()
			keys := keyapp.NewService("rate-capacity", relational.NewClientKeyRepository(db), limiter, memory.NewConcurrencyLimiter(), 60, 5, cipher)
			t.Cleanup(func() {
				closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := keys.Close(closeCtx); err != nil {
					t.Error(err)
				}
			})
			secrets := make([]string, 4)
			for i, rpm := range []int{1, 2, 1, 0} {
				v, e := keys.Create(ctx, keyapp.CreateInput{Name: fmt.Sprintf("rate-%d", i), Enabled: true, RPMLimit: rpm, RPMUnlimited: rpm == 0, ConcurrencyUnlimited: true})
				if e != nil {
					t.Fatal(e)
				}
				secrets[i] = v.Secret
			}
			admins := adminapp.NewService(relational.NewAdminRepository(db), relational.NewAdminSessionRepository(db), security.NewTokenService("rate-capacity-fixture-signing-key"), time.Minute, time.Hour)
			admins.SetLoginRateLimiter(limiter)
			if err := admins.Bootstrap(ctx, "rate-owner", "fixture-password"); err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			arrived := 0
			for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
				router.POST(path, middleware.ClientAuth(keys), func(c *gin.Context) { arrived++; c.Status(http.StatusNoContent) })
			}
			adminhttp.NewHandler(admins, false).RegisterPublic(router.Group("/api"))
			remote := "192.0.2.8:1234"
			request := func(path, secret, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if secret != "" {
					r.Header.Set("Authorization", "Bearer "+secret)
				}
				r.RemoteAddr = remote
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				return w
			}
			for i := range 2 {
				if w := request("/v1/responses", secrets[i], "{}"); w.Code != 204 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			// Prime the real administrator's fixed username window through the same
			// runtime port. The HTTP request below must still be refused before bcrypt.
			for range 12 {
				if ok, _, e := limiter.Allow(ctx, "admin-login:user:"+security.HashToken("rate-owner"), 12, time.Now()); e != nil || !ok {
					t.Fatal(ok, e)
				}
			}
			if ok, _, e := limiter.Allow(ctx, "admin-login:ip:"+security.HashToken("192.0.2.8"), 30, time.Now()); e != nil || !ok {
				t.Fatal(ok, e)
			}
			for i := range 40000 {
				_, _, _ = limiter.Allow(ctx, fmt.Sprintf("pressure:%d", i), 1, time.Now())
			}
			for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
				w := request(path, secrets[0], "{}")
				if w.Code != 429 || w.Header().Get("Retry-After") == "" || (path != "/v1/messages" && !strings.Contains(w.Body.String(), "rate_limit_exceeded")) {
					t.Errorf("spent key %s: %d %s", path, w.Code, w.Body.String())
				}
			}
			w := request("/v1/responses", secrets[2], "{}")
			if w.Code != 503 || !strings.Contains(w.Body.String(), "runtime_store_unavailable") || w.Header().Get("Retry-After") != "" {
				t.Errorf("untracked key: %d %s", w.Code, w.Body.String())
			}
			if w = request("/v1/messages", secrets[1], "{}"); w.Code != 204 {
				t.Errorf("retained unused quota: %d %s", w.Code, w.Body.String())
			}
			if w = request("/v1/messages", secrets[1], "{}"); w.Code != 429 {
				t.Errorf("retained quota exhausted: %d %s", w.Code, w.Body.String())
			}
			if w = request("/v1/chat/completions", secrets[3], "{}"); w.Code != 204 {
				t.Errorf("explicit unlimited: %d %s", w.Code, w.Body.String())
			}
			if arrived != 4 {
				t.Errorf("protected handler calls=%d, want 4", arrived)
			}
			if w = request("/api/auth/login", "", `{"username":"rate-owner","password":"fixture-password"}`); w.Code != 429 || w.Header().Get("Retry-After") == "" {
				t.Errorf("admin spent quota: status=%d", w.Code)
			}
			remote = "192.0.2.9:1234"
			if w = request("/api/auth/login", "", `{ "username":"rate-owner", "password":"fixture-password" }`); w.Code != 503 {
				t.Errorf("admin new window capacity: status=%d", w.Code)
			}
		})
	}
}
