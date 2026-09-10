package relational

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAdminPasswordAndSessionsAtomicAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			ra, rb := NewAdminRepository(a), NewAdminRepository(b)
			sa, sb := NewAdminSessionRepository(a), NewAdminSessionRepository(b)
			current, err := ra.Create(ctx, admin.Admin{Username: "admin-atomic", PasswordHash: "original-fixture-hash"})
			if err != nil {
				t.Fatal(err)
			}
			ref := current.PasswordRef()
			create := func(s *AdminSessionRepository, i int) (admin.Session, error) {
				return s.CreateForPassword(ctx, ref, security.HashToken(fmt.Sprintf("fixture-%d", i)), time.Now().UTC().Add(time.Hour))
			}
			// Seed the bound, then exercise concurrent creators on both pools.
			for i := range admin.MaxSessions {
				if _, err := create(sa, i); err != nil {
					t.Fatal(err)
				}
			}
			var wg sync.WaitGroup
			errs := make(chan error, 12)
			for i := range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s := sa
					if i%2 != 0 {
						s = sb
					}
					_, err := create(s, admin.MaxSessions+i)
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			count := func() int64 {
				t.Helper()
				var n int64
				if err := b.db.Model(&adminSessionModel{}).Where("admin_id = ?", current.ID).Count(&n).Error; err != nil {
					t.Fatal(err)
				}
				return n
			}
			if n := count(); n != admin.MaxSessions {
				t.Fatalf("session bound exceeded: %d", n)
			}
			// Failure of session revocation must roll back the password change.
			if err := a.db.Callback().Delete().Before("gorm:delete").Register("fail_admin_revoke", func(tx *gorm.DB) {
				if tx.Statement.Table == "admin_sessions" {
					tx.AddError(errors.New("injected session delete failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := ra.UpdatePasswordAndRevokeSessions(ctx, ref, "failed-new-hash"); err == nil {
				t.Fatal("revocation error committed password")
			}
			if err := a.db.Callback().Delete().Remove("fail_admin_revoke"); err != nil {
				t.Fatal(err)
			}
			unchanged, err := rb.GetByID(ctx, current.ID)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.PasswordHash != ref.Hash || count() != admin.MaxSessions {
				t.Fatal("password and revocation were not atomic")
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := sb.CreateForPassword(cancelled, ref, security.HashToken("cancelled"), time.Now().UTC().Add(time.Hour)); err == nil {
				t.Fatal("cancelled creation accepted")
			}
			if count() != admin.MaxSessions {
				t.Fatal("cancelled creator evicted a session")
			}
			if err := rb.UpdatePasswordAndRevokeSessions(ctx, ref, "current-new-hash"); err != nil {
				t.Fatal(err)
			}
			if count() != 0 {
				t.Fatal("password change left old sessions")
			}
			if _, err := create(sa, 1000); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("stale login=%v", err)
			}
			if err := ra.UpdatePasswordAndRevokeSessions(ctx, ref, "stale-late-hash"); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("stale password change=%v", err)
			}
			fresh, err := rb.GetByID(ctx, current.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sa.CreateForPassword(ctx, fresh.PasswordRef(), security.HashToken("fresh"), time.Now().UTC().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if count() != 1 {
				t.Fatal("fresh login failed")
			}
			// A committed refresh whose cookie has not arrived must remain
			// revocable by the browser's immediately previous token.
			freshSession, err := sb.GetByTokenHash(ctx, security.HashToken("fresh"))
			if err != nil {
				t.Fatal(err)
			}
			if err := sa.Rotate(ctx, freshSession.ID, security.HashToken("fresh"), security.HashToken("rotated"), time.Now().UTC().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := sb.RevokeByTokenHash(ctx, ""); err != nil || count() != 1 {
				t.Fatal("empty token matched unset previous hashes")
			}
			if err := sb.RevokeByTokenHash(ctx, security.HashToken("unrelated")); err != nil || count() != 1 {
				t.Fatal("unrelated token revoked a session")
			}
			if err := sb.RevokeByTokenHash(ctx, security.HashToken("fresh")); err != nil || count() != 0 {
				t.Fatalf("previous-token logout failed: %v", err)
			}

		})
	}
}
