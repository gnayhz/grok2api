package model

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Measures only local orchestration/SQL on an existing account and catalog.
// Upstream discovery is in memory; this is not an upstream throughput claim.
func BenchmarkAccountCapabilitySync(b *testing.B) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "model-sync.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		b.Fatal(err)
	}
	token, err := cipher.Encrypt("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	accounts := relational.NewAccountRepository(db)
	v, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "bench", SourceKey: "bench", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		b.Fatal(err)
	}
	registry := provider.NewRegistry(&modelCapabilityAdapter{models: map[uint64][]string{v.ID: {"model-a", "model-b", "model-c"}}})
	as := accountapp.NewService(accounts, relational.NewAuditRepository(db), memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, nil)
	service := NewService(relational.NewModelRepository(db), accounts, as, registry)
	if _, err := service.SyncAccount(ctx, v.ID); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := service.SyncAccount(ctx, v.ID); err != nil {
			b.Fatal(err)
		}
	}
}
