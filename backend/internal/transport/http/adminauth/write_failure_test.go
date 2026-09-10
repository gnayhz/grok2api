package adminauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adminapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

var errAuthWrite = errors.New("injected private database detail")

type authSessionFault struct {
	repository.AdminSessionRepository
	operation string
}

func (s *authSessionFault) CreateForPassword(ctx context.Context, expected admin.PasswordRef, hash string, expires time.Time) (admin.Session, error) {
	if s.operation == "create" {
		return admin.Session{}, errAuthWrite
	}
	return s.AdminSessionRepository.CreateForPassword(ctx, expected, hash, expires)
}
func (s *authSessionFault) Rotate(ctx context.Context, id uint64, old, next string, expires time.Time) error {
	if s.operation == "rotate" {
		return errAuthWrite
	}
	return s.AdminSessionRepository.Rotate(ctx, id, old, next, expires)
}
func (s *authSessionFault) Revoke(ctx context.Context, id uint64) error {
	if s.operation == "revoke" {
		return errAuthWrite
	}
	return s.AdminSessionRepository.Revoke(ctx, id)
}
func (s *authSessionFault) GetByPreviousTokenHash(ctx context.Context, hash string) (admin.Session, error) {
	v, err := s.AdminSessionRepository.GetByPreviousTokenHash(ctx, hash)
	past := time.Now().UTC().Add(-time.Minute)
	v.LastUsedAt = &past
	return v, err
}

type authPasswordFault struct{ repository.AdminRepository }

func (s authPasswordFault) UpdatePasswordAndRevokeSessions(context.Context, admin.PasswordRef, string) error {
	return errAuthWrite
}

func TestAuthWriteFailuresReturnUnavailableWithoutCookies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, operation := range []string{"create", "rotate", "revoke", "password"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			sessions := &authSessionFault{AdminSessionRepository: relational.NewAdminSessionRepository(db)}
			svc := adminapp.NewService(authPasswordFault{relational.NewAdminRepository(db)}, sessions, security.NewTokenService("12345678901234567890123456789012"), time.Minute, time.Hour)
			if err := svc.Bootstrap(ctx, "admin", "original-password"); err != nil {
				t.Fatal(err)
			}
			_, token, err := svc.Login(ctx, "admin", "original-password", "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			if operation == "revoke" {
				if _, err := svc.Refresh(ctx, token.RefreshToken); err != nil {
					t.Fatal(err)
				}
			}
			sessions.operation = operation
			router := gin.New()
			root := router.Group("/api/admin/v1")
			h := NewHandler(svc, true)
			h.RegisterPublic(root)
			protected := root.Group("")
			protected.Use(middleware.AdminAuth(svc))
			h.RegisterAuthenticated(protected)
			method, path, body := http.MethodPost, "/auth/refresh", `{"refreshToken":"`+token.RefreshToken+`"}`
			if operation == "create" {
				path, body = "/auth/login", `{"username":"admin","password":"original-password"}`
			}
			if operation == "password" {
				method, path, body = http.MethodPut, "/me/password", `{"currentPassword":"original-password","newPassword":"new-password"}`
			}
			req := httptest.NewRequest(method, "/api/admin/v1"+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token.AccessToken)
			out := httptest.NewRecorder()
			router.ServeHTTP(out, req)
			if out.Code != http.StatusServiceUnavailable || !strings.Contains(out.Body.String(), `"authRuntimeUnavailable"`) || strings.Contains(out.Body.String(), "private database") || len(out.Result().Cookies()) != 0 {
				t.Fatalf("unsafe/unclassified failure: status=%d cookies=%d body=%s", out.Code, len(out.Result().Cookies()), out.Body.String())
			}
		})
	}
}
