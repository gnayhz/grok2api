package account

import (
	"context"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestDurableQuotaRecoveryKeepsRetryBudgetAndAccountDeletion(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "durable-quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	credential, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, Name: "pending", SourceKey: "pending", AuthType: accountdomain.AuthTypeSSO, EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{}
	service := NewService(repo, nil, nil, nil, providerimpl.NewRegistry(adapter), nil, security.RandomTokenSource{}, nil, nil, nil)
	fact := accountdomain.QuotaConsumption{EventID: "pending-quota", AccountID: credential.ID, Mode: "fast", Units: 1}
	if receipt, err := repo.ConsumeQuota(ctx, fact, time.Now().UTC()); err != nil || receipt.State != accountdomain.QuotaConsumptionPendingRefresh {
		t.Fatalf("pending=%+v %v", receipt, err)
	}
	if cursor := service.recoverDurableQuotaRefreshes(ctx, 0); cursor != credential.ID || len(service.quotaRefresh.queue) != 1 {
		t.Fatalf("persisted work not discovered: cursor=%d queue=%d", cursor, len(service.quotaRefresh.queue))
	}
	request := <-service.quotaRefresh.queue
	state := service.quotaRefresh.obs[request.key]
	state.queued = false
	state.failures = quotaRefreshFailureBudget
	service.requeueQuotaRefreshes()
	for range 5 {
		service.recoverDurableQuotaRefreshes(ctx, 0)
		service.requeueQuotaRefreshes()
	}
	if len(service.quotaRefresh.queue) != 0 || state.pending || state.failures != quotaRefreshFailureBudget || !state.durable {
		t.Fatalf("background scan reset retry episode: %+v", state)
	}
	if pending, err := repo.ListPendingQuotaRefreshes(ctx, 0, 100); err != nil || len(pending) != 1 {
		t.Fatalf("parking lost durable demand: %+v %v", pending, err)
	}
	service.QueueQuotaRefresh(credential.ID, "fast")
	request = <-service.quotaRefresh.queue
	service.runQuotaRefresh(ctx, request)
	if adapter.modeCalls.Load() != 1 {
		t.Fatalf("explicit demand did not resume once: %d", adapter.modeCalls.Load())
	}
	if receipt, err := repo.ConsumeQuota(ctx, fact, time.Now().UTC()); err != nil || receipt.State != accountdomain.QuotaConsumptionRefreshed {
		t.Fatalf("refresh did not resolve accepted fact: %+v %v", receipt, err)
	}
	// Group replacement may remove the old per-product mode. The query still
	// resolves that pending demand rather than retaining an impossible counter.
	fact.EventID, fact.Mode = "removed-product", accountdomain.QuotaModeWebVideo
	if _, err := repo.ConsumeQuota(ctx, fact, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	revision, err := repo.GetQuotaRevision(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: credential.ID, Revision: revision, SyncedAt: time.Now().UTC(), ReplaceModes: accountdomain.WebImagineQuotaModes()}); err != nil {
		t.Fatal(err)
	}
	if receipt, err := repo.ConsumeQuota(ctx, fact, time.Now().UTC()); err != nil || receipt.State != accountdomain.QuotaConsumptionRefreshed {
		t.Fatalf("absent product remained pending: %+v %v", receipt, err)
	}
	fact.EventID = "deleted-account-pending"
	if _, err := repo.ConsumeQuota(ctx, fact, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	service.recoverDurableQuotaRefreshes(ctx, 0)
	if pending, err := repo.ListPendingQuotaRefreshes(ctx, 0, 100); err != nil || len(pending) != 0 {
		t.Fatalf("deleted account queued forever: %+v %v", pending, err)
	}
	if receipt, err := repo.ConsumeQuota(ctx, fact, time.Now().UTC()); err != nil || receipt.State != accountdomain.QuotaConsumptionAccountDeleted {
		t.Fatalf("deleted demand receipt: %+v %v", receipt, err)
	}
}
