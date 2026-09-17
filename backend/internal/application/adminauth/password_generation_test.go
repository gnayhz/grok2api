package adminauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type pausedPasswordRead struct {
	repository.AdminRepository
	byName  bool
	read    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *pausedPasswordRead) wait() { p.once.Do(func() { close(p.read); <-p.release }) }
func (p *pausedPasswordRead) GetByUsername(ctx context.Context, name string) (admin.Admin, error) {
	v, err := p.AdminRepository.GetByUsername(ctx, name)
	if p.byName && err == nil {
		p.wait()
	}
	return v, err
}
func (p *pausedPasswordRead) GetByID(ctx context.Context, id uint64) (admin.Admin, error) {
	v, err := p.AdminRepository.GetByID(ctx, id)
	if !p.byName && err == nil {
		p.wait()
	}
	return v, err
}

func TestPasswordChangeFencesEarlierPasswordVerification(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, action := range []string{"login", "password_change"} {
			t.Run(dialect+"/"+action, func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "auth.db")
				open := func() (*relational.Database, error) { return relational.OpenSQLite(ctx, path) }
				if dialect == "postgres" {
					dsn := os.Getenv("TEST_POSTGRES_DSN")
					if dsn == "" {
						t.Skip("isolated PostgreSQL DSN not set")
					}
					open = func() (*relational.Database, error) { return relational.OpenPostgres(ctx, dsn, 4, 4) }
				}
				a, err := open()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = a.Close() })
				if err := a.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
				b, err := open()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = b.Close() })
				ra, rb := relational.NewAdminRepository(a), relational.NewAdminRepository(b)
				hash, err := security.NewBCryptPasswordHasher().HashPassword("original-password")
				if err != nil {
					t.Fatal(err)
				}
				name := fmt.Sprintf("password-fence-%d", time.Now().UnixNano())
				identity, err := ra.Create(ctx, admin.Admin{Username: name, PasswordHash: hash})
				if err != nil {
					t.Fatal(err)
				}
				paused := &pausedPasswordRead{AdminRepository: ra, byName: action == "login", read: make(chan struct{}), release: make(chan struct{})}
				tokens := security.NewTokenService("12345678901234567890123456789012")
				first := NewService(paused, relational.NewAdminSessionRepository(a), tokens, security.NewBCryptPasswordHasher(), security.RandomTokenSource{}, time.Minute, time.Hour)
				second := NewService(rb, relational.NewAdminSessionRepository(b), tokens, security.NewBCryptPasswordHasher(), security.RandomTokenSource{}, time.Minute, time.Hour)
				var release sync.Once
				unblock := func() { release.Do(func() { close(paused.release) }) }
				defer unblock()
				type result struct {
					tokens Tokens
					err    error
				}
				done := make(chan result, 1)
				go func() {
					if action == "login" {
						_, token, err := first.Login(ctx, name, "original-password", "127.0.0.1")
						done <- result{token, err}
						return
					}
					done <- result{err: first.ChangePassword(ctx, identity.ID, "original-password", "late-password")}
				}()
				select {
				case <-paused.read:
				case <-time.After(3 * time.Second):
					t.Fatal("old password read never started")
				}
				if err := second.ChangePassword(ctx, identity.ID, "original-password", "current-password"); err != nil {
					t.Fatal(err)
				}
				unblock()
				var out result
				select {
				case out = <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("old password operation never finished")
				}
				if !errors.Is(out.err, ErrInvalidCredentials) {
					if action == "login" && out.err == nil {
						_, authErr := second.AuthenticateAccess(ctx, out.tokens.AccessToken)
						t.Fatalf("old password created session after revocation barrier: login_err=%v access_valid=%v", out.err, authErr == nil)
					}
					t.Fatalf("old password operation crossed change barrier: err=%v", out.err)
				}
				if _, _, err := second.Login(ctx, name, "current-password", "127.0.0.1"); err != nil {
					t.Fatalf("current password was overwritten: %v", err)
				}
			})
		}
	}
}

func TestLogoutRevokesConcurrentlyRotatedFamily(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "logout.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	svc := NewService(relational.NewAdminRepository(db), relational.NewAdminSessionRepository(db), security.NewTokenService("12345678901234567890123456789012"), security.NewBCryptPasswordHasher(), security.RandomTokenSource{}, time.Minute, time.Hour)
	if err := svc.Bootstrap(ctx, "admin", "original-password"); err != nil {
		t.Fatal(err)
	}
	_, first, err := svc.Login(ctx, "admin", "original-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	// A refresh has committed; its response/cookies have not reached the caller
	// whose logout request still carries the previously valid token.
	rotated, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, first.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateAccess(ctx, rotated.AccessToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("logout accepted but rotated family survives: %v", err)
	}
}
