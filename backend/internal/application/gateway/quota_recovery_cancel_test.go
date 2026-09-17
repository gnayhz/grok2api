package gateway

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestKnownQuotaExhaustionSurvivesClientCancellation(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "cancel", SourceKey: "cancel", EncryptedAccessToken: "token", Enabled: true, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	sel := selector.NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	// The upstream response has already established this fact. Client transport
	// cancellation must not erase it before the independent account write.
	sel.MarkFreeQuotaExhausted(canceled, v, 100, 100)
	got, err := repo.GetQuotaRecovery(ctx, v.ID)
	if err != nil || got.ConfirmedUsed != 100 {
		t.Fatalf("known exhaustion lost after cancellation: used=%d err=%v", got.ConfirmedUsed, err)
	}
}
