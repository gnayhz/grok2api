package adminauth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// round 61 回归：超过 bcrypt 72 字节上限的新密码必须在服务层被归类为
// ErrInvalidPassword（传输层 400），而不是让 bcrypt 的 ErrPasswordTooLong
// 落到 500 分支。72 字节边界本身仍需可用。
func TestChangePasswordRejectsOverlongPasswordAsInvalid(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "pw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := NewService(
		relational.NewAdminRepository(database),
		relational.NewAdminSessionRepository(database),
		security.NewTokenService("12345678901234567890123456789012"),
		15*time.Minute,
		30*24*time.Hour,
	)
	if err := service.Bootstrap(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	adminValue, err := service.admins.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, adminValue.ID, "password123", string(make([]byte, 73))); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("73-byte password: err = %v, want ErrInvalidPassword", err)
	}
	if err := service.ChangePassword(ctx, adminValue.ID, "password123", string(make([]byte, 1000))); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("1000-byte password: err = %v, want ErrInvalidPassword", err)
	}
	// 恰好 72 字节必须成功（bcrypt 上限内）。
	if err := service.ChangePassword(ctx, adminValue.ID, "password123", string(make([]byte, 72))); err != nil {
		t.Fatalf("72-byte password should be accepted: %v", err)
	}
}

// Bootstrap 同样受 72 字节上限保护。
func TestBootstrapRejectsOverlongPassword(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "pw2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := NewService(
		relational.NewAdminRepository(database),
		relational.NewAdminSessionRepository(database),
		security.NewTokenService("12345678901234567890123456789012"),
		15*time.Minute,
		30*24*time.Hour,
	)
	if err := service.Bootstrap(ctx, "admin", string(make([]byte, 80))); !errors.Is(err, ErrBootstrapRequired) {
		t.Fatalf("overlong bootstrap password must be rejected before hashing: %v", err)
	}
}
