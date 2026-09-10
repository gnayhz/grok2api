package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestModelRestrictionsKeepIndependentFactsAndObservedMaterial(t *testing.T) {
	for _, scenario := range []string{"quota_reset_preserves_same_model_denial", "late_quota_after_reset", "old_denial_after_reimport"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-restriction.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
			v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "model", SourceKey: "model", EncryptedAccessToken: "old-token", Enabled: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "quota_reset_preserves_same_model_denial":
				if err := selector.MarkModelAccessDenied(ctx, v, "same-model", time.Hour); err != nil {
					t.Fatal(err)
				}
				selector.MarkModelQuotaExhausted(ctx, v, nil, "same-model", 24*time.Hour)
				if err := repo.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
					t.Fatal(err)
				}
			case "late_quota_after_reset":
				if err := repo.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
					t.Fatal(err)
				}
				selector.MarkModelQuotaExhausted(ctx, v, nil, "same-model", 24*time.Hour)
			case "old_denial_after_reimport":
				replacement := v
				replacement.EncryptedAccessToken = "new-token"
				if _, _, err := repo.UpsertByIdentity(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				if err := selector.MarkModelAccessDenied(ctx, v, "same-model", time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			candidates, err := repo.ListRoutingCandidates(ctx, v.Provider, 0, "same-model", "")
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates=%d err=%v", len(candidates), err)
			}
			block := candidates[0].ModelQuotaBlock
			if scenario == "quota_reset_preserves_same_model_denial" {
				if block == nil || block.Reason != "model_access_denied" {
					t.Fatalf("quota reset erased same-model independent denial: %+v", block)
				}
			} else if block != nil {
				t.Fatalf("obsolete outcome blocked current model: %+v", block)
			}
		})
	}
}

func TestModelRestrictionFactsSurviveClientCancellation(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "cancel-model.db"))
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
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := selector.MarkModelAccessDenied(canceled, v, "model", time.Hour); err != nil {
		t.Fatal(err)
	}
	selector.MarkModelQuotaExhausted(canceled, v, nil, "model", 24*time.Hour)
	candidates, err := repo.ListRoutingCandidates(ctx, v.Provider, 0, "model", "")
	if err != nil || len(candidates) != 1 || candidates[0].ModelQuotaBlock == nil || candidates[0].ModelQuotaBlock.Reason != "model_quota_depleted" {
		t.Fatalf("known quota lost after cancel: %+v %v", candidates, err)
	}
	if err := repo.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
		t.Fatal(err)
	}
	candidates, err = repo.ListRoutingCandidates(ctx, v.Provider, 0, "model", "")
	if err != nil || len(candidates) != 1 || candidates[0].ModelQuotaBlock == nil || candidates[0].ModelQuotaBlock.Reason != "model_access_denied" {
		t.Fatalf("known denial lost after cancel: %+v %v", candidates, err)
	}
}
