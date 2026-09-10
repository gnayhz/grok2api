package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	gatewayapp "github.com/chenyme/grok2api/backend/internal/application/gateway"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/gin-gonic/gin"
)

func TestQuotaResetHTTPPreservesSelectorRestrictions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, all := range []bool{false, true} {
		for _, restriction := range []string{"health", "quality", "risk", "model_access", "authentication", "disabled"} {
			t.Run(fmt.Sprintf("all=%t/%s", all, restriction), func(t *testing.T) {
				ctx := context.Background()
				db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-http.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if err := db.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
				repo := relational.NewAccountRepository(db)
				selector := gatewayapp.NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
				repo.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { selector.ApplyInvalidation(e) })
				v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, Name: "reset-http", SourceKey: "reset-http", EncryptedAccessToken: "token", Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 100, MaxConcurrent: 1})
				if err != nil {
					t.Fatal(err)
				}
				acquire := func(wantAvailable bool) {
					t.Helper()
					lease, err := selector.Acquire(ctx, v.Provider, 0, "quota-model", "", "", map[uint64]bool{}, false)
					if err == nil {
						lease.Release()
					}
					if (err == nil) != wantAvailable {
						t.Fatalf("selector available=%t want=%t err=%v", err == nil, wantAvailable, err)
					}
				}
				acquire(true) // Warm a real candidate cache before the independent restriction.
				switch restriction {
				case "health", "quality":
					event := accountdomain.HealthEvent{Kind: accountdomain.HealthFailure, Status: 503, CooldownBase: time.Hour, CooldownMax: time.Hour}
					if restriction == "quality" {
						event = accountdomain.HealthEvent{Kind: accountdomain.HealthQualityIdle, RetryAfter: time.Hour}
					}
					_, err = repo.ApplyHealth(ctx, v.ID, v.Provider, event)
				case "risk":
					err = repo.UpdateRiskAttribution(ctx, v.ID, repository.RiskAttribution{Status: accountdomain.RiskStatusRSCDenied, Trigger: accountdomain.RiskTriggerManual})
				case "model_access":
					err = testsupport.ModelRestriction(ctx, repo, accountdomain.ModelQuotaBlock{AccountID: v.ID, UpstreamModel: "quota-model", Reason: "model_access_denied", CooldownUntil: time.Now().Add(2 * time.Hour)})
				case "authentication":
					_, err = repo.ApplyCredential(ctx, v.CredentialRef(), accountdomain.CredentialEvent{Kind: accountdomain.CredentialRejected, Reason: "reimport required"})
				case "disabled":
					enabled := false
					_, err = repo.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &enabled}})
				}
				if err != nil {
					t.Fatal(err)
				}
				if restriction == "model_access" {
					// Same-model quota can outlast denial but reset must retain
					// the independent access fact and invalidate a warmed cache.
					selector.MarkModelQuotaExhausted(ctx, v, nil, "quota-model", 24*time.Hour)
				}
				until := time.Now().Add(time.Hour)
				if err := testsupport.Recovery(ctx, repo, accountdomain.QuotaRecovery{AccountID: v.ID, Kind: accountdomain.QuotaRecoveryKindFree, Status: accountdomain.QuotaRecoveryStatusExhausted, NextProbeAt: &until}); err != nil {
					t.Fatal(err)
				}
				acquire(false)
				service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, nil, nil, nil)
				router := gin.New()
				NewHandler(service, nil).Register(router.Group("/api/admin/v1"))
				server := httptest.NewServer(router)
				defer server.Close()
				path := "/api/admin/v1/accounts/batch/reset-quota"
				body := fmt.Sprintf(`{"provider":"grok_build","ids":["%d"]}`, v.ID)
				wantReset := 1
				if all {
					path = "/api/admin/v1/accounts/reset-quota"
					body = ""
					if restriction == "authentication" || restriction == "disabled" {
						wantReset = 0
					}
				}
				response, err := server.Client().Post(server.URL+path, "application/json", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Data struct {
						Reset int `json:"reset"`
					} `json:"data"`
				}
				decodeErr := json.NewDecoder(response.Body).Decode(&result)
				response.Body.Close()
				if response.StatusCode != http.StatusOK || decodeErr != nil || result.Data.Reset != wantReset {
					t.Fatalf("reset HTTP status=%d reset=%d want=%d decode=%v", response.StatusCode, result.Data.Reset, wantReset, decodeErr)
				}
				acquire(false)
				if restriction == "health" || restriction == "quality" {
					response, err := server.Client().Post(fmt.Sprintf("%s/api/admin/v1/accounts/%d/clear-cooldown-force", server.URL, v.ID), "application/json", nil)
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					if response.StatusCode != http.StatusOK {
						t.Fatalf("explicit cooldown clear status=%d", response.StatusCode)
					}
					acquire(true)
				}
			})
		}
	}
}
