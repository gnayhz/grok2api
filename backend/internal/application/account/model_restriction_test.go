package account

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestBuildDetectModelRestrictionsRespectIndependentFactsAndGeneration(t *testing.T) {
	for _, scenario := range []string{"same_model_reasons", "late_quota_reset", "old_denial_material", "canceled_quota", "canceled_denial"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "detect-model.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, Name: "detect", SourceKey: "detect", EncryptedAccessToken: "token", Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(repo, nil, nil, nil, nil, nil, nil)
			apply := func(callCtx context.Context, denial bool) {
				t.Helper()
				body := `{"error":"You've used all the included free usage for model grok-4.5"}`
				if denial {
					body = `{"error":"Access to the chat endpoint is denied"}`
				}
				item := service.finishBuildDetectResponse(callCtx, &provider.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(body))}, v, nil)
				if item.Outcome != BuildDetectOutcomeFailed {
					t.Fatalf("outcome=%s reason=%s", item.Outcome, item.Reason)
				}
			}
			wantReason := ""
			switch scenario {
			case "same_model_reasons":
				apply(ctx, true)
				apply(ctx, false)
				if err := repo.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
					t.Fatal(err)
				}
				wantReason = "model_access_denied"
			case "late_quota_reset":
				if err := repo.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
					t.Fatal(err)
				}
				apply(ctx, false)
			case "old_denial_material":
				replacement := v
				replacement.EncryptedAccessToken = "replacement"
				if _, _, err := repo.UpsertByIdentity(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				apply(ctx, true)
			default:
				callCtx, cancel := context.WithCancel(ctx)
				cancel()
				denial := scenario == "canceled_denial"
				apply(callCtx, denial)
				wantReason = "model_quota_depleted"
				if denial {
					wantReason = "model_access_denied"
				}
			}
			candidates, err := repo.ListRoutingCandidates(ctx, v.Provider, 0, buildDetectModel, "")
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates=%d err=%v", len(candidates), err)
			}
			got := candidates[0].ModelQuotaBlock
			if wantReason == "" {
				if got != nil {
					t.Fatalf("obsolete result persisted: %+v", got)
				}
			} else if got == nil || got.Reason != wantReason || !got.CooldownUntil.After(time.Now()) {
				t.Fatalf("restriction=%+v want=%s", got, wantReason)
			}
		})
	}
}
