package adminauth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var errSessionWriteFixture = errors.New("injected session write failure")

type failingSessionWrite struct {
	repository.AdminSessionRepository
	operation string
}

func (s failingSessionWrite) CreateForPassword(ctx context.Context, expected admin.PasswordRef, tokenHash string, expiresAt time.Time) (admin.Session, error) {
	if s.operation == "create" {
		return admin.Session{}, errSessionWriteFixture
	}
	return s.AdminSessionRepository.CreateForPassword(ctx, expected, tokenHash, expiresAt)
}
func (s failingSessionWrite) Rotate(ctx context.Context, id uint64, previous, next string, expires time.Time) error {
	if s.operation == "rotate" {
		return errSessionWriteFixture
	}
	return s.AdminSessionRepository.Rotate(ctx, id, previous, next, expires)
}
func (s failingSessionWrite) Revoke(ctx context.Context, id uint64) error {
	if s.operation == "revoke" {
		return errSessionWriteFixture
	}
	return s.AdminSessionRepository.Revoke(ctx, id)
}
func (s failingSessionWrite) GetByPreviousTokenHash(ctx context.Context, hash string) (admin.Session, error) {
	v, err := s.AdminSessionRepository.GetByPreviousTokenHash(ctx, hash)
	// Simulate arrival after the existing replay grace without a wall-clock wait.
	past := time.Now().UTC().Add(-time.Minute)
	v.LastUsedAt = &past
	return v, err
}

func TestSessionWriteFailureIsUnavailable(t *testing.T) {
	for _, operation := range []string{"create", "rotate", "revoke"} {
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
			service := NewService(relational.NewAdminRepository(db), failingSessionWrite{relational.NewAdminSessionRepository(db), operation}, security.NewTokenService("12345678901234567890123456789012"), security.NewBCryptPasswordHasher(), security.RandomTokenSource{}, time.Minute, time.Hour)
			if err := service.Bootstrap(ctx, "admin", "original-password"); err != nil {
				t.Fatal(err)
			}
			_, token, err := service.Login(ctx, "admin", "original-password", "127.0.0.1")
			if operation != "create" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = service.Refresh(ctx, token.RefreshToken)
				if operation == "revoke" {
					if err != nil {
						t.Fatal(err)
					}
					_, err = service.Refresh(ctx, token.RefreshToken)
				}
			}
			if !errors.Is(err, ErrRuntimeUnavailable) {
				t.Fatalf("%s write failure misclassified: %v", operation, err)
			}
		})
	}
}
