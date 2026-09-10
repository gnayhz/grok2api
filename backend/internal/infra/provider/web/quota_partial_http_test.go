package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

func TestWebFullQuotaHTTPRejectsIncompleteBasicSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db := partialQuotaDatabase(t, driver)
			for _, failedMode := range []string{"auto", "fast"} {
				for _, failure := range []string{"503", "malformed", "timeout", "unknown_tier"} {
					t.Run(failedMode+"_"+failure, func(t *testing.T) {
						ctx := context.Background()
						repo := relational.NewAccountRepository(db)
						cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
						if err != nil {
							t.Fatal(err)
						}
						token, err := cipher.Encrypt("synthetic-quota-credential")
						if err != nil {
							t.Fatal(err)
						}
						value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, AuthStatus: account.AuthStatusActive, Enabled: true, Name: "partial", SourceKey: failedMode + "_" + failure, WebTier: account.WebTierBasic, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", Email: "synthetic@example.test", EncryptedAccessToken: token})
						if err != nil {
							t.Fatal(err)
						}
						revision, err := repo.GetQuotaRevision(ctx, value.ID)
						if err != nil {
							t.Fatal(err)
						}
						now := time.Now().UTC()
						reset := now.Add(time.Hour)
						windows := []account.QuotaWindow{{AccountID: value.ID, Mode: "auto", Remaining: 0, Total: 7, ResetAt: &reset, Source: account.QuotaSourceUpstream}, {AccountID: value.ID, Mode: "fast", Remaining: 0, Total: 30, ResetAt: &reset, Source: account.QuotaSourceUpstream}}
						if err := repo.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: value.ID, Revision: revision, Tier: account.WebTierBasic, SyncedAt: now, Windows: windows, ReplaceAll: true}); err != nil {
							t.Fatal(err)
						}
						before, err := repo.GetQuotaWindows(ctx, []uint64{value.ID})
						if err != nil {
							t.Fatal(err)
						}
						beforeRevision, err := repo.GetQuotaRevision(ctx, value.ID)
						if err != nil {
							t.Fatal(err)
						}
						beforeAccount, err := repo.Get(ctx, value.ID)
						if err != nil {
							t.Fatal(err)
						}
						var healthy atomic.Bool
						var modeCalls, imagineCalls, unexpectedCalls atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/rest/media/imagine/quota_info" {
								imagineCalls.Add(1)
								_, _ = io.Copy(io.Discard, r.Body)
								writeEmptyImagineQuota(w)
								return
							}
							if r.URL.Path != "/rest/rate-limits" {
								unexpectedCalls.Add(1)
								http.NotFound(w, r)
								return
							}
							var input struct {
								ModelName string `json:"modelName"`
							}
							if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
								t.Error(err)
								return
							}
							modeCalls.Add(1)
							if !healthy.Load() && input.ModelName == failedMode {
								switch failure {
								case "malformed":
									w.Header().Set("Content-Type", "application/json")
									_, _ = io.WriteString(w, `{"remainingQueries":`)
								case "timeout":
									<-r.Context().Done()
								default:
									http.Error(w, "temporary quota failure", http.StatusServiceUnavailable)
								}
								return
							}
							total := map[string]int{"auto": 7, "fast": 30}[input.ModelName]
							if failure == "unknown_tier" && !healthy.Load() {
								total = 9
							}
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(map[string]int{"remainingQueries": 3, "totalQueries": total, "windowSizeSeconds": 7200})
						}))
						defer upstream.Close()
						manager := infraegress.NewManager(relational.NewEgressRepository(db), cipher)
						defer manager.Close(ctx)
						adapter := NewAdapter(Config{BaseURL: upstream.URL, QuotaTimeout: 250 * time.Millisecond, StatsigMode: "manual", StatsigManualValue: base64.RawStdEncoding.EncodeToString(make([]byte, 70))}, manager, cipher, nil, nil)
						service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, provider.NewRegistry(adapter), cipher, nil)
						router := gin.New()
						accounthttp.NewHandler(service, nil).Register(router.Group("/api/admin/v1"))
						response := httptest.NewRecorder()
						router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/v1/accounts/%d/refresh-quota", value.ID), nil))
						after, err := repo.GetQuotaWindows(ctx, []uint64{value.ID})
						if err != nil {
							t.Fatal(err)
						}

						afterRevision, err := repo.GetQuotaRevision(ctx, value.ID)
						if err != nil {
							t.Fatal(err)
						}
						afterAccount, err := repo.Get(ctx, value.ID)
						if err != nil {
							t.Fatal(err)
						}
						if response.Code != http.StatusBadGateway || !reflect.DeepEqual(before, after) || afterRevision != beforeRevision || !reflect.DeepEqual(beforeAccount, afterAccount) {
							t.Fatalf("partial snapshot reported success or mutated authority: HTTP=%d revision=%d->%d before=%v after=%v", response.Code, beforeRevision, afterRevision, before[value.ID], after[value.ID])
						}
						if modeCalls.Load() != 2 || imagineCalls.Load() != 1 || unexpectedCalls.Load() != 0 {
							t.Fatalf("partial request counts modes=%d imagine=%d unexpected=%d", modeCalls.Load(), imagineCalls.Load(), unexpectedCalls.Load())
						}
						healthy.Store(true)
						response = httptest.NewRecorder()
						router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/v1/accounts/%d/refresh-quota", value.ID), nil))
						after, err = repo.GetQuotaWindows(ctx, []uint64{value.ID})
						if err != nil {
							t.Fatal(err)
						}
						if response.Code != http.StatusOK || len(after[value.ID]) != 2 {
							t.Fatalf("complete followup: HTTP=%d windows=%v", response.Code, after[value.ID])
						}
						for _, window := range after[value.ID] {
							if window.Remaining != 3 {
								t.Fatalf("complete followup window=%+v", window)
							}
						}
						if modeCalls.Load() != 4 || imagineCalls.Load() != 2 || unexpectedCalls.Load() != 0 {
							t.Fatalf("followup counts modes=%d imagine=%d unexpected=%d", modeCalls.Load(), imagineCalls.Load(), unexpectedCalls.Load())
						}
					})
				}
			}
		})
	}
}

func partialQuotaDatabase(t *testing.T, driver string) *relational.Database {
	t.Helper()
	ctx := context.Background()
	var db *relational.Database
	var err error
	if driver == "sqlite" {
		db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "partial-quota.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("requires isolated TEST_POSTGRES_DSN")
		}
		admin, connectErr := pgx.Connect(ctx, dsn)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		t.Cleanup(func() { _ = admin.Close(ctx) })
		schema := fmt.Sprintf("web_partial_quota_%d", time.Now().UnixNano())
		if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
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
		db, err = relational.OpenPostgres(ctx, parsed.String(), 4, 4)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}
