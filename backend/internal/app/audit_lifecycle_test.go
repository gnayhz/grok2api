package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type delayedAuditSQL struct {
	repository.AuditRepository
	entered chan struct{}
	release chan struct{}
}

func (r *delayedAuditSQL) CreateBatch(context.Context, []audit.Record) error {
	close(r.entered)
	<-r.release
	return nil
}

func TestApplicationCloseWaitsForAuditBeforeClosingDependencies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pending.db")
	options := relational.AuditJournalOptions{MaxRecords: 8, MaxBytes: 1 << 20}
	journal, err := relational.OpenAuditJournal(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	repo := &delayedAuditSQL{AuditRepository: relational.NewAuditRepository(database), entered: make(chan struct{}), release: make(chan struct{})}
	writer := auditapp.NewService(repo, journal, nil, 4, time.Millisecond)
	if err := writer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	application := &Application{database: database, audits: writer, auditJournal: journal}
	result := make(chan error, 1)
	go func() {
		result <- writer.Create(ctx, audit.Record{EventID: "evt_application_close", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200})
	}()
	select {
	case <-repo.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closed := make(chan error, 1)
	go func() { closed <- application.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("application closed while writer owns SQL: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if _, _, err := repo.List(ctx, 0, 1); err != nil {
		t.Fatalf("main storage closed early: %v", err)
	}
	if other, err := relational.OpenAuditJournal(ctx, path, options); !errors.Is(err, repository.ErrAuditPendingOwner) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("pending ownership released early: %v", err)
	}
	close(repo.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, auditapp.ErrWriterUnavailable) {
		t.Fatalf("stopped caller result: %v", err)
	}
	if _, _, err := repo.List(ctx, 0, 1); err == nil {
		t.Fatal("main storage stayed open after stopped writer")
	}
	reopened, err := relational.OpenAuditJournal(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Snapshot().Records != 1 {
		t.Fatal("shutdown lost the fact whose SQL acknowledgement was interrupted")
	}
}

func TestConstructionFailureReleasesDatabaseHandles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor ownership check uses Linux procfs")
	}
	for _, phase := range []string{"cipher", "media", "runtime", "bootstrap"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			raw := "secrets:\n  jwtSecret: '12345678901234567890123456789012'\n  credentialEncryptionKey: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n"
			if err := os.WriteFile(configPath, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "cipher":
				cfg.Secrets.CredentialEncryptionKey = "invalid-cipher"
			case "media":
				blocked := filepath.Join(dir, "file-not-directory")
				if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
				cfg.Media.Local.Path = filepath.Join(blocked, "media")
			case "runtime":
				cfg.RuntimeStore.Driver = "injected-unsupported-driver"
			case "bootstrap":
				cfg.BootstrapAdmin.Username = ""
			}
			if app, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
				app.Close()
				t.Fatal("constructor unexpectedly succeeded")
			}
			files, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			var retained []string
			for _, file := range files {
				path, err := os.Readlink(filepath.Join("/proc/self/fd", file.Name()))
				if err == nil && strings.HasPrefix(path, dir+string(filepath.Separator)) {
					retained = append(retained, path)
				}
			}
			if len(retained) > 0 {
				t.Fatalf("failed construction retained storage handles: %v", retained)
			}
		})
	}
}

func TestConstructionWithLiteralSQLitePathKeepsQualityInSameFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	raw := "secrets:\n  jwtSecret: '12345678901234567890123456789012'\n  credentialEncryptionKey: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n"
	if err := os.WriteFile(configPath, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.SQLite.Path = filepath.Join(dir, "literal #?%.db")
	cfg.BootstrapAdmin.Username = "test-admin"
	cfg.BootstrapAdmin.Password = "fixture-admin-password"
	application, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if !application.quality.DB().Migrator().HasTable("provider_accounts") {
		t.Fatal("quality could not observe main account schema")
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		files, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			path, err := os.Readlink(filepath.Join("/proc/self/fd", file.Name()))
			if err == nil && strings.HasPrefix(path, dir+string(filepath.Separator)) {
				t.Fatalf("closed application retained %q", path)
			}
		}
	}
}
