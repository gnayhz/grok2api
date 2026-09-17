package accountsync_test

import (
	"context"
	"database/sql"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// Only upstream protocol parsing is synthetic. The maintenance policy, initial
// pipeline, model observations, publication and persistence are real consumers.
type initialHTTPAdapter struct {
	url    string
	client *http.Client
}

func (*initialHTTPAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }
func (*initialHTTPAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: accountdomain.ProviderBuild, Quota: provider.QuotaBilling, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth, Import: true, Refresh: true}}
}
func (a *initialHTTPAdapter) query(ctx context.Context, c accountdomain.Credential, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url+path+"?id="+strconv.FormatUint(c.ID, 10), nil)
	if err != nil {
		return err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if _, err = io.Copy(io.Discard, res.Body); err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream HTTP %d", res.StatusCode)
	}
	return nil
}
func (a *initialHTTPAdapter) GetBilling(ctx context.Context, c accountdomain.Credential) (accountdomain.Billing, error) {
	err := a.query(ctx, c, "/billing")
	return accountdomain.Billing{AccountID: c.ID, SyncedAt: time.Now().UTC()}, err
}
func (a *initialHTTPAdapter) ListModels(ctx context.Context, c accountdomain.Credential) ([]string, error) {
	return []string{"grok-initial"}, a.query(ctx, c, "/models")
}
func (*initialHTTPAdapter) RefreshCredential(context.Context, accountdomain.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, fmt.Errorf("unexpected refresh")
}
func (*initialHTTPAdapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	return []provider.CredentialSeed{{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: string(data), SourceKey: string(data), AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour)}}, nil
}
func (*initialHTTPAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}

type initialFixture struct {
	db                       *relational.Database
	accounts                 *relational.AccountRepository
	maintenance              *accountapp.Service
	models                   *modelapp.Service
	service                  *accountsyncapp.Service
	pool                     *batch.Pool
	encrypted                string
	billingCalls, modelCalls atomic.Int64
}

func initialDatabase(t testing.TB, dialect string) *relational.Database {
	t.Helper()
	ctx := context.Background()
	var db *relational.Database
	var err error
	if dialect == "sqlite" {
		db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "initial.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("isolated TEST_POSTGRES_DSN required")
		}
		admin, openErr := sql.Open("pgx", dsn)
		if openErr != nil {
			t.Fatal(openErr)
		}
		schema := fmt.Sprintf("g28_initial_%d", time.Now().UnixNano())
		if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer admin.Close()
			if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		db, err = relational.OpenPostgres(ctx, parsed.String(), 8, 4)
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
func newInitialFixture(t testing.TB, dialect string, handler http.HandlerFunc) *initialFixture {
	t.Helper()
	f := &initialFixture{db: initialDatabase(t, dialect), pool: batch.NewPool(4)}
	f.accounts = relational.NewAccountRepository(f.db)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	f.encrypted, err = cipher.Encrypt("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing":
			f.billingCalls.Add(1)
		case "/models":
			f.modelCalls.Add(1)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if handler != nil {
			handler(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)
	adapter := &initialHTTPAdapter{url: server.URL, client: server.Client()}
	registry := providerimpl.NewRegistry(adapter)
	f.maintenance = accountapp.NewService(f.accounts, relational.NewAuditRepository(f.db), nil, nil, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
	f.models = modelapp.NewService(relational.NewModelRepository(f.db), f.accounts, f.maintenance, registry)
	f.service = accountsyncapp.NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), f.maintenance, f.maintenance, f.maintenance, f.maintenance, f.maintenance, f.maintenance, f.maintenance, f.models)
	f.service.SetBulkPool(f.pool)
	return f
}
func (f *initialFixture) account(t testing.TB, name string) uint64 {
	t.Helper()
	c, _, err := f.accounts.UpsertByIdentity(context.Background(), accountdomain.Credential{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, SourceKey: name, Name: name, EncryptedAccessToken: f.encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}
func (f *initialFixture) assertSnapshot(t testing.TB, id uint64, billing, models bool) {
	t.Helper()
	b, err := f.maintenance.HasBillingSnapshot(context.Background(), id)
	if err != nil || b != billing {
		t.Fatalf("billing snapshot=%v err=%v", b, err)
	}
	m, err := f.models.HasSuccessfulAccountSync(context.Background(), id)
	if err != nil || m != models {
		t.Fatalf("model snapshot=%v err=%v", m, err)
	}
}
func awaitInitial[T any](t testing.TB, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("initial pipeline did not finish")
		var zero T
		return zero
	}
}
func waitInitial(t testing.TB, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("initial pipeline condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
func initialGate() (chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}
func startInitial(s *accountsyncapp.Service, ctx context.Context, ids ...uint64) <-chan accountsyncapp.Result {
	done := make(chan accountsyncapp.Result, 1)
	go func() { done <- s.Sync(ctx, ids...) }()
	return done
}
func assertInitial(t testing.TB, result accountsyncapp.Result, succeeded, failed int) {
	t.Helper()
	if result.Succeeded != succeeded || result.Failed != failed {
		t.Fatalf("result=%+v want success=%d failed=%d", result, succeeded, failed)
	}
}
