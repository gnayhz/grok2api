package app

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/jackc/pgx/v5"
)

func TestProviderDurationWiringPreservesConfiguration(t *testing.T) {
	base, err := loadGuardPolicyFile(t, "  enabled: true\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, fraction := range []time.Duration{0, 500 * time.Millisecond, time.Nanosecond} {
		t.Run(fraction.String(), func(t *testing.T) {
			cfg := base
			cfg.Provider.Web.QuotaTimeout = config.Duration(time.Second + fraction)
			cfg.Provider.Web.ChatTimeout = config.Duration(time.Minute + fraction)
			cfg.Provider.Web.StreamIdleTimeout = config.Duration(30*time.Second + fraction)
			cfg.Provider.Web.ImageTimeout = config.Duration(time.Minute + fraction)
			cfg.Provider.Web.VideoTimeout = config.Duration(2*time.Minute + fraction)
			cfg.Provider.Console.ChatTimeout = config.Duration(time.Minute + fraction)
			cfg.Provider.Console.StreamIdleTimeout = config.Duration(30*time.Second + fraction)
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			web, console := webProviderConfig(cfg), consoleProviderConfig(cfg)
			for _, tc := range []struct {
				name string
				got  time.Duration
				want time.Duration
			}{
				{"web_quota", web.QuotaTimeout, cfg.Provider.Web.QuotaTimeout.Value()},
				{"web_chat", web.ChatTimeout, cfg.Provider.Web.ChatTimeout.Value()},
				{"web_idle", web.StreamIdleTimeout, cfg.Provider.Web.StreamIdleTimeout.Value()},
				{"web_image", web.ImageTimeout, cfg.Provider.Web.ImageTimeout.Value()},
				{"web_video", web.VideoTimeout, cfg.Provider.Web.VideoTimeout.Value()},
				{"console_chat", console.Timeout, cfg.Provider.Console.ChatTimeout.Value()},
				{"console_idle", console.StreamIdleTimeout, cfg.Provider.Console.StreamIdleTimeout.Value()},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.got != tc.want {
						t.Fatalf("validated configuration changed during provider handoff: got=%s want=%s", tc.got, tc.want)
					}
				})
			}
		})
	}
}

func TestApplicationProviderDurationStartupUpdateReload(t *testing.T) {
	// A child process trusts only this fixture's additional CA file, before
	// crypto/x509 initializes its process-wide roots. Production TLS checks and
	// the settings HTTPS validation remain active; other tests keep their roots.
	if os.Getenv("GROK_TEST_PROVIDER_DURATION_CHILD") != "1" {
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		certificate := server.Certificate().Raw
		server.Close()
		rootsPath := filepath.Join(t.TempDir(), "upstream-ca.pem")
		if err := os.WriteFile(rootsPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0600); err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestApplicationProviderDurationStartupUpdateReload$", "-test.v", "-test.timeout=110s")
		cmd.Env = append(os.Environ(), "GROK_TEST_PROVIDER_DURATION_CHILD=1", "SSL_CERT_FILE="+rootsPath)
		output, err := cmd.CombinedOutput()
		t.Log(string(output))
		if err != nil {
			t.Fatalf("isolated TLS integration: %v", err)
		}
		return
	}
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			sharedPath := filepath.Join(t.TempDir(), "shared.db")
			dsn := ""
			if driver == "postgres" {
				dsn = os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated TEST_POSTGRES_DSN")
				}
				admin, err := pgx.Connect(ctx, dsn)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = admin.Close(ctx) })
				schema := fmt.Sprintf("provider_duration_%d", time.Now().UnixNano())
				if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
						t.Error(err)
					}
				})
				parsed, err := url.Parse(dsn)
				if err != nil {
					t.Fatal(err)
				}
				query := parsed.Query()
				query.Set("search_path", schema)
				parsed.RawQuery = query.Encode()
				dsn = parsed.String()
			}
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				timer := time.NewTimer(1400 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-r.Context().Done():
					return
				case <-timer.C:
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"remainingQueries":7,"totalQueries":10,"windowSizeSeconds":3600}`)
			}))
			defer server.Close()
			configure := func(cfg *config.Config) {
				cfg.Database.Driver = driver
				cfg.Database.SQLite.Path = sharedPath
				cfg.Database.Postgres.DSN = dsn
				cfg.Provider.Web.BaseURL = server.URL
				cfg.Provider.Web.StatsigMode = "manual"
				cfg.Provider.Web.StatsigManualValue = base64.RawStdEncoding.EncodeToString(make([]byte, 70))
				cfg.Provider.Web.QuotaTimeout = config.Duration(1900 * time.Millisecond)
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			a := newLifecycleApplication(t, configure)
			b := newLifecycleApplication(t, configure)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			token, err := cipher.Encrypt("synthetic-duration-credential")
			if err != nil {
				t.Fatal(err)
			}
			credential := account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: token}
			check := func(t *testing.T, app *Application, timeout bool) {
				t.Helper()
				prior := calls.Load()
				got, err := app.web.SyncQuotaMode(ctx, credential, "auto")
				if calls.Load() != prior+1 {
					t.Fatalf("upstream call count: %d -> %d", prior, calls.Load())
				}
				if timeout {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("updated 1.1s deadline was not applied: %v", err)
					}
				} else if err != nil || got.Remaining != 7 {
					t.Fatalf("configured 1.9s deadline lost response: remaining=%d err=%v", got.Remaining, err)
				}
			}
			t.Run("startup", func(t *testing.T) { check(t, a, false) })
			snapshot := a.settings.Get()
			input := snapshot.Config
			input.ProviderWeb.QuotaTimeout = "1.1s"
			if _, err := a.settings.Update(ctx, snapshot.Revision, input); err != nil {
				t.Fatal(err)
			}
			t.Run("hot_update_shorter", func(t *testing.T) { check(t, a, true) })
			snapshot = a.settings.Get()
			input = snapshot.Config
			input.ProviderWeb.QuotaTimeout = "1.9s"
			input.ProviderWeb.ChatTimeout = "1m0.000000001s"
			input.ProviderWeb.StreamIdleTimeout = "30.5s"
			input.ProviderWeb.ImageTimeout = "1m0.5s"
			input.ProviderWeb.VideoTimeout = "2m0.5s"
			input.ProviderConsole.ChatTimeout = "1m0.5s"
			input.ProviderConsole.StreamIdleTimeout = "30.5s"
			saved, err := a.settings.Update(ctx, snapshot.Revision, input)
			if err != nil {
				t.Fatal(err)
			}
			if saved.ApplyPending || saved.AppliedRevision != saved.Revision {
				t.Fatalf("apply incomplete: revision=%d applied=%d", saved.Revision, saved.AppliedRevision)
			}
			t.Run("hot_update_longer", func(t *testing.T) { check(t, a, false) })
			if err := b.settings.ReloadPersisted(ctx); err != nil {
				t.Fatal(err)
			}
			verify := func(t *testing.T, current *Application) {
				t.Helper()
				loaded := current.settings.Get()
				if loaded.Revision != saved.Revision || loaded.ApplyPending || loaded.Config.ProviderWeb != saved.Config.ProviderWeb || loaded.Config.ProviderConsole != saved.Config.ProviderConsole {
					t.Fatalf("durable provider configuration changed: revision=%d want=%d", loaded.Revision, saved.Revision)
				}
				check(t, current, false)
			}
			t.Run("second_instance_reload", func(t *testing.T) { verify(t, b) })
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			c := newLifecycleApplication(t, configure)
			t.Run("restart", func(t *testing.T) { verify(t, c) })
		})
	}
}

func TestWebQuotaUsesConfiguredFractionalDeadline(t *testing.T) {
	base, err := loadGuardPolicyFile(t, "  enabled: true\n")
	if err != nil {
		t.Fatal(err)
	}
	base.Provider.Web.QuotaTimeout = config.Duration(1900 * time.Millisecond)
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "provider-duration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base.Secrets.CredentialEncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("synthetic-duration-credential")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		timer := time.NewTimer(1400 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"remainingQueries":7,"totalQueries":10,"windowSizeSeconds":3600}`)
	}))
	defer server.Close()
	manager := infraegress.NewManager(relational.NewEgressRepository(db), cipher)
	defer manager.Close(context.Background())
	// Only the local protocol endpoint and deterministic signer differ from
	// production wiring; SQL routing, lease, HTTP transport and adapter are real.
	cfg := webProviderConfig(base)
	cfg.BaseURL, cfg.StatsigMode, cfg.StatsigManualValue = server.URL, "manual", "synthetic-signature"
	adapter := webprovider.NewAdapter(cfg, manager, cipher, nil, nil)
	got, err := adapter.SyncQuotaMode(ctx, account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: token}, "auto")
	if err != nil || got.Remaining != 7 || calls.Load() != 1 {
		t.Fatalf("response inside configured 1.9s deadline was lost: remaining=%d calls=%d err=%v", got.Remaining, calls.Load(), err)
	}
}
