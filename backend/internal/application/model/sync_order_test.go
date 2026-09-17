package model

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"path/filepath"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Independent services share real SQL while the earlier upstream call is held.
// A later success must remain the routing snapshot after either an old success
// or an old failure returns, including when account material was reimported.
func TestAccountCapabilitySyncRejectsLateResults(t *testing.T) {
	for _, reimport := range []bool{false, true} {
		for _, failOld := range []bool{false, true} {
			name := "same_material"
			if reimport {
				name = "reimported_material"
			}
			if failOld {
				name += "/late_failure"
			} else {
				name += "/late_success"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "capability-order.db")
				db, err := relational.OpenSQLite(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				if err := db.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
				peer, err := relational.OpenSQLite(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = peer.Close() })
				cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
				if err != nil {
					t.Fatal(err)
				}
				token, err := cipher.Encrypt("original")
				if err != nil {
					t.Fatal(err)
				}
				accounts := relational.NewAccountRepository(db)
				credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "ordered", SourceKey: "ordered", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				old := &lateCapabilityAdapter{modelCapabilityAdapter: &modelCapabilityAdapter{models: map[uint64][]string{credential.ID: {"old-model"}}, entered: make(chan struct{}), release: make(chan struct{})}, fail: failOld}
				makeService := func(database *relational.Database, adapter provider.Adapter) *Service {
					ar := relational.NewAccountRepository(database)
					registry := providerimpl.NewRegistry(adapter)
					as := accountapp.NewService(ar, relational.NewAuditRepository(database), memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
					return NewService(relational.NewModelRepository(database), ar, as, registry)
				}
				older := makeService(db, old)
				newer := makeService(peer, &modelCapabilityAdapter{models: map[uint64][]string{credential.ID: {"new-model"}}})
				var release sync.Once
				defer release.Do(func() { close(old.release) })
				done := make(chan error, 1)
				go func() { _, err := older.SyncAccount(ctx, credential.ID); done <- err }()
				select {
				case <-old.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("older sync did not start")
				}
				if reimport {
					credential.EncryptedAccessToken, err = cipher.Encrypt("replacement")
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := accounts.UpsertByIdentity(ctx, credential); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := newer.SyncAccount(ctx, credential.ID); err != nil {
					t.Fatal(err)
				}
				release.Do(func() { close(old.release) })
				select {
				case err := <-done:
					if !failOld && !errors.Is(err, modeldomain.ErrCapabilitySyncSuperseded) {
						t.Errorf("late success did not report superseded observation: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("older sync did not finish")
				}
				if _, err := newer.models.GetByPublicID(ctx, "new-model"); err != nil {
					t.Errorf("new snapshot lost routing availability: %v", err)
				}
				if _, err := newer.models.GetByPublicID(ctx, "old-model"); !errors.Is(err, repository.ErrNotFound) {
					t.Errorf("late result published old routing snapshot: %v", err)
				}
				readDB, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer readDB.Close()
				var lastError string
				if err := readDB.QueryRowContext(ctx, "SELECT last_error FROM account_model_sync_states WHERE account_id = ?", credential.ID).Scan(&lastError); err != nil {
					t.Fatal(err)
				}
				if lastError != "" {
					t.Errorf("late failure replaced newer successful diagnostic: %q", lastError)
				}
			})
		}
	}
}

type lateCapabilityAdapter struct {
	*modelCapabilityAdapter
	fail bool
}

func (a *lateCapabilityAdapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	values, err := a.modelCapabilityAdapter.ListModels(ctx, credential)
	if a.fail {
		return nil, errors.New("old discovery failed")
	}
	return values, err
}

func TestAccountCapabilitySyncKeepsClaimThroughCredentialRefresh(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "capability-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("expired")
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(db)
	v, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "refresh", SourceKey: "refresh", EncryptedAccessToken: token, EncryptedRefreshToken: token, ExpiresAt: time.Now().Add(-time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &refreshingCapabilityAdapter{modelCapabilityAdapter: &modelCapabilityAdapter{models: map[uint64][]string{v.ID: {"refreshed-model"}}}}
	registry := providerimpl.NewRegistry(adapter)
	as := accountapp.NewService(accounts, relational.NewAuditRepository(db), memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
	models := relational.NewModelRepository(db)
	service := NewService(models, accounts, as, registry)
	if count, err := service.SyncAccount(ctx, v.ID); err != nil || count != 1 {
		t.Fatalf("refreshed sync: count=%d err=%v", count, err)
	}
	current, err := accounts.Get(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.CredentialGeneration != v.CredentialGeneration+1 || adapter.observed != current.CredentialRef() {
		t.Fatal("ListModels did not use refreshed material")
	}
	if _, err := models.GetByPublicID(ctx, "refreshed-model"); err != nil {
		t.Fatal(err)
	}
	// Exactly one claim was consumed by the whole credential/model operation.
	next, err := models.BeginAccountCapabilitySync(ctx, v.ID, time.Now())
	if err != nil || next.Revision != 2 {
		t.Fatalf("refresh restarted its claim: next=%v err=%v", next, err)
	}
}

type refreshingCapabilityAdapter struct {
	*modelCapabilityAdapter
	observed account.CredentialRef
}

func (a *refreshingCapabilityAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: account.ProviderBuild, Credential: provider.CredentialSurface{Refresh: true}}
}

func (a *refreshingCapabilityAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{EncryptedAccessToken: "refreshed", EncryptedRefreshToken: "rotated-refresh", ExpiresAt: time.Now().Add(time.Hour), RefreshTokenRotated: true}, nil
}

func (a *refreshingCapabilityAdapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	a.observed = credential.CredentialRef()
	return a.modelCapabilityAdapter.ListModels(ctx, credential)
}
