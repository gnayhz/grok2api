package account

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func currentImportService(t *testing.T, adapters ...provider.Adapter) (*Service, *relational.AccountRepository) {
	t.Helper()
	db, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "current-import.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	return NewService(repo, nil, memory.NewDeviceSessionStore(), nil, provider.NewRegistry(adapters...), cipher, memory.NewLockStore()), repo
}

func currentImportCredential(t *testing.T, s *Service, p accountdomain.Provider, key, email string) accountdomain.Credential {
	t.Helper()
	auth := accountdomain.AuthTypeSSO
	if p == accountdomain.ProviderBuild {
		auth = accountdomain.AuthTypeOAuth
	}
	seed := provider.CredentialSeed{Provider: p, AuthType: auth, Name: key, SourceKey: key, Email: email, AccessToken: "synthetic"}
	if auth == accountdomain.AuthTypeOAuth {
		seed.RefreshToken = "synthetic-refresh"
	}
	value, err := s.credentialFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := s.accounts.UpsertByIdentity(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type currentPreparedAdapter struct {
	rtImportAdapter
	prepare func(provider.CredentialSeed) provider.CredentialSeed
}

func (a currentPreparedAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return []provider.CredentialSeed{{Name: "prepared", SourceKey: "prepared", OIDCClientID: "client", RefreshToken: "old-refresh"}}, nil
}

func (a currentPreparedAdapter) PrepareImportedCredential(_ context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	return a.prepare(seed), nil
}

func TestPreparedImportChecksCurrentDeletionAfterExchange(t *testing.T) {
	for _, timing := range []string{"email_discovered_after_prefilter", "delete_and_cancel_during_exchange"} {
		t.Run(timing, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			adapter := &currentPreparedAdapter{}
			s, repo := currentImportService(t, adapter)
			v := currentImportCredential(t, s, accountdomain.ProviderBuild, "deleted", "prepared@example.test")
			if timing == "email_discovered_after_prefilter" {
				if n, err := s.BatchDelete(ctx, []uint64{v.ID}); err != nil || n != 1 {
					t.Fatalf("delete: %d, %v", n, err)
				}
			}
			adapter.prepare = func(seed provider.CredentialSeed) provider.CredentialSeed {
				if timing == "delete_and_cancel_during_exchange" {
					if n, err := s.BatchDelete(ctx, []uint64{v.ID}); err != nil || n != 1 {
						t.Fatalf("delete during exchange: %d, %v", n, err)
					}
					cancel()
				}
				seed.Email, seed.AccessToken, seed.RefreshToken = v.Email, "new-access", "rotated-refresh"
				return seed
			}
			observed := 0
			var progress [][2]int
			out, err := s.ImportCredentialsWithProgress(ctx, []byte("synthetic"), func(uint64) error { observed++; return nil }, func(done, total int) error {
				progress = append(progress, [2]int{done, total})
				return nil
			})
			if timing == "delete_and_cancel_during_exchange" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected request cancellation, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if out.Created != 0 || out.Updated != 0 || out.Skipped != 1 || len(out.AccountIDs) != 0 || observed != 0 || !reflect.DeepEqual(progress, [][2]int{{0, 1}, {1, 1}}) {
				t.Fatalf("prepared deletion outcome: %+v, observed=%d, progress=%v", out, observed, progress)
			}
			values, err := repo.ListEnabled(context.Background(), accountdomain.ProviderBuild)
			if err != nil || len(values) != 0 {
				t.Fatalf("prepared import resurrected account: count=%d, %v", len(values), err)
			}
		})
	}
}

type currentImportPort struct {
	repository.AccountRepository
	beforeImport func()
}

func (p *currentImportPort) ImportAccounts(ctx context.Context, inputs []repository.AccountImport) ([]repository.AccountUpsertResult, error) {
	if p.beforeImport != nil {
		fn := p.beforeImport
		p.beforeImport = nil
		fn()
	}
	return p.AccountRepository.ImportAccounts(ctx, inputs)
}

func TestWebSynchronizationChecksCurrentSource(t *testing.T) {
	for _, destination := range []string{"console", "build"} {
		for _, change := range []string{"delete_unknown_email", "rotate", "delete_known_email_peer"} {
			t.Run(destination+"/"+change, func(t *testing.T) {
				s, repo := currentImportService(t, consoleSSOCodecAdapter{}, &buildConversionAdapter{})
				ctx := context.Background()
				v := currentImportCredential(t, s, accountdomain.ProviderWeb, "source", "")
				port := &currentImportPort{AccountRepository: repo}
				s.accounts = port
				port.beforeImport = func() {
					switch change {
					case "delete_unknown_email":
						if n, err := s.BatchDelete(ctx, []uint64{v.ID}); err != nil || n != 1 {
							t.Fatalf("delete source: %d, %v", n, err)
						}
					case "rotate":
						if _, _, err := repo.UpsertByIdentity(ctx, v); err != nil {
							t.Fatal(err)
						}
					case "delete_known_email_peer":
						peer := currentImportCredential(t, s, accountdomain.ProviderBuild, "peer", "shared@example.test")
						if _, err := repo.ApplyIdentity(ctx, v.CredentialRef(), accountdomain.IdentityObservation{Email: peer.Email}); err != nil {
							t.Fatal(err)
						}
						if n, err := s.BatchDelete(ctx, []uint64{peer.ID}); err != nil || n != 1 {
							t.Fatalf("delete peer: %d, %v", n, err)
						}
					}
				}
				observed := 0
				observer := func(uint64) error { observed++; return nil }
				if destination == "console" {
					var progress [][2]int
					out, err := s.SyncWebAccountsToConsoleWithStrategy(ctx, []uint64{v.ID}, WebConsoleSyncMissing, observer, func(done, total int) error {
						progress = append(progress, [2]int{done, total})
						return nil
					})
					if err != nil || out.Created != 0 || out.Updated != 0 || out.Skipped != 1 || len(out.AccountIDs) != 0 || !reflect.DeepEqual(progress, [][2]int{{0, 1}, {1, 1}}) {
						t.Fatalf("console sync: %+v, progress=%v, %v", out, progress, err)
					}
				} else {
					out, err := s.ConvertWebAccountsToBuildWithObserver(ctx, []uint64{v.ID}, observer)
					if err != nil || out.Created != 0 || out.Linked != 0 || out.Skipped != 1 || out.Failed != 0 || len(out.BuildAccountIDs) != 0 {
						t.Fatalf("build conversion: %+v, %v", out, err)
					}
				}
				if observed != 0 {
					t.Fatal("skipped synchronization notified imported observer")
				}
			})
		}
	}
}

func TestImportChunkReportsCommittedAndSkippedResults(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "progress", true: "observer_failure"}[interrupt], func(t *testing.T) {
			s, _ := currentImportService(t, rtImportAdapter{})
			ctx := context.Background()
			deleted := currentImportCredential(t, s, accountdomain.ProviderBuild, "deleted", "deleted@example.test")
			if _, err := s.BatchDelete(ctx, []uint64{deleted.ID}); err != nil {
				t.Fatal(err)
			}
			seeds := []provider.CredentialSeed{
				{Name: "blocked", SourceKey: "blocked", Email: deleted.Email, AccessToken: "synthetic"},
				{Name: "one", SourceKey: "one", AccessToken: "synthetic"},
				{Name: "two", SourceKey: "two", AccessToken: "synthetic"},
			}
			observed := 0
			var progress [][2]int
			interrupted := errors.New("observer interrupted")
			out, err := s.persistImportedSeeds(ctx, seeds, func(uint64) error {
				observed++
				if interrupt {
					return interrupted
				}
				return nil
			}, func(done, total int) error { progress = append(progress, [2]int{done, total}); return nil })
			if interrupt && !errors.Is(err, interrupted) || !interrupt && err != nil {
				t.Fatalf("outcome error: %v", err)
			}
			if out.Created != 2 || out.Updated != 0 || out.Skipped != 1 || len(out.AccountIDs) != 2 {
				t.Fatalf("lost committed chunk result: %+v", out)
			}
			if !interrupt && (observed != 2 || !reflect.DeepEqual(progress, [][2]int{{0, 3}, {1, 3}, {2, 3}, {3, 3}})) {
				t.Fatalf("callback contract: observed=%d, progress=%v", observed, progress)
			}
		})
	}
}

type currentDeviceAdapter struct {
	rtImportAdapter
	seed provider.CredentialSeed
}

func (currentDeviceAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }
func (currentDeviceAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{DeviceCode: "synthetic", ExpiresIn: time.Minute, Interval: time.Second}, nil
}
func (a currentDeviceAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return a.seed, nil
}

func TestDeviceLoginCannotRecreateDeletedIdentity(t *testing.T) {
	adapter := &currentDeviceAdapter{seed: provider.CredentialSeed{Name: "device", SourceKey: "device", Email: "device@example.test", AccessToken: "synthetic"}}
	s, _ := currentImportService(t, adapter)
	ctx := context.Background()
	v := currentImportCredential(t, s, accountdomain.ProviderBuild, "deleted", adapter.seed.Email)
	if _, err := s.BatchDelete(ctx, []uint64{v.ID}); err != nil {
		t.Fatal(err)
	}
	started, err := s.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.deviceSessions.Get(ctx, started.SessionID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return session.NextPollAt }
	if _, err := s.PollDeviceLogin(ctx, started.SessionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("device login bypassed deletion: %v", err)
	}
	if _, err := s.deviceSessions.Get(ctx, started.SessionID, time.Now()); err == nil {
		t.Fatal("terminally rejected device session retained")
	}
}
