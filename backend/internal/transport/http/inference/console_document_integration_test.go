package inference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

func incompleteConsoleDocument(body []byte) []byte {
	return append(append(body, strings.Repeat(" ", provider.MaxDiagnosticBodyBytes-len(body))...), []byte("{broken tail")...)
}

func TestHTTPConsoleTruncatedTokenCannotAuthorizeGeneration(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", driver, stream), func(t *testing.T) {
				var healthy atomic.Bool
				var mints atomic.Int32
				f := newResponseRetentionFixture(t, driver, account.ProviderConsole, responseRetentionHooks{tokenBody: func(body []byte) []byte {
					mints.Add(1)
					if !healthy.Load() {
						return incompleteConsoleDocument(body)
					}
					return body
				}})
				result := f.create(t, stream, nil, "truncated")
				if result.Code < 400 || f.generated.Load() != 0 || mints.Load() != 1 {
					t.Fatalf("status=%d mint=%d generated=%d body=%s", result.Code, mints.Load(), f.generated.Load(), result.Body.String())
				}
				record := f.lastAudit(t)
				if record.InputTokens != 0 || record.OutputTokens != 0 || len(record.GenerationUsages) != 0 {
					t.Fatalf("invalid token charged: %+v", record)
				}
				healthy.Store(true)
				// An explicit administrator recovery allows a fresh attempt after the
				// malformed upstream response's existing health cooldown.
				if _, err := f.accountService.ClearCooldown(context.Background(), f.accountID); err != nil {
					t.Fatal(err)
				}
				f.restartGateway()
				retentionResponse(t, f.create(t, stream, nil, "recovered"), stream)
				retentionResponse(t, f.create(t, stream, nil, "cached"), stream)
				if mints.Load() != 2 || f.generated.Load() != 2 {
					t.Fatalf("mint=%d generated=%d", mints.Load(), f.generated.Load())
				}
				record = f.lastAudit(t)
				if record.InputTokens != 20 || record.OutputTokens != 5 || record.LedgerOutcome != "committed" {
					t.Fatalf("recovered completion lost: %+v", record)
				}
			})
		}
	}
}

func TestHTTPConsoleTruncatedUsagePreservesAuthority(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			var healthy atomic.Bool
			var usageCalls atomic.Int32
			f := newResponseRetentionFixture(t, driver, account.ProviderConsole, responseRetentionHooks{beforeUsage: func(w http.ResponseWriter, r *http.Request) bool {
				usageCalls.Add(1)
				body := []byte(`{"quotas":[{"kind":"chat","limit":10,"used":1,"remaining":9},{"kind":"image","limit":5,"used":0,"remaining":5},{"kind":"video","limit":2,"used":0,"remaining":2}]}`)
				if !healthy.Load() {
					body = incompleteConsoleDocument(body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
				return true
			}})
			ctx := context.Background()
			revision, err := f.accounts.GetQuotaRevision(ctx, f.accountID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			reset := now.Add(time.Hour)
			windows := []account.QuotaWindow{}
			for _, mode := range []string{"console", "console_image", "console_video", "legacy"} {
				windows = append(windows, account.QuotaWindow{AccountID: f.accountID, Mode: mode, Remaining: 0, Total: 7, ResetAt: &reset, Source: account.QuotaSourceUpstream})
			}
			if err := f.accounts.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: f.accountID, Revision: revision, SyncedAt: now, Windows: windows, ReplaceAll: true}); err != nil {
				t.Fatal(err)
			}
			before, err := f.accounts.GetQuotaWindows(ctx, []uint64{f.accountID})
			if err != nil {
				t.Fatal(err)
			}
			beforeRevision, err := f.accounts.GetQuotaRevision(ctx, f.accountID)
			if err != nil {
				t.Fatal(err)
			}
			beforeAccount, err := f.accounts.Get(ctx, f.accountID)
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			accounthttp.NewHandler(f.accountService, nil).Register(router.Group("/api/admin/v1"))
			refresh := func() *httptest.ResponseRecorder {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/v1/accounts/%d/refresh-quota", f.accountID), nil))
				return response
			}
			result := refresh()
			after, err := f.accounts.GetQuotaWindows(ctx, []uint64{f.accountID})
			if err != nil {
				t.Fatal(err)
			}
			afterRevision, err := f.accounts.GetQuotaRevision(ctx, f.accountID)
			if err != nil {
				t.Fatal(err)
			}
			afterAccount, err := f.accounts.Get(ctx, f.accountID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Code != http.StatusBadGateway || !reflect.DeepEqual(before, after) || beforeRevision != afterRevision || !reflect.DeepEqual(beforeAccount, afterAccount) || usageCalls.Load() != 1 {
				t.Fatalf("truncated usage changed authority: status=%d revision=%d->%d calls=%d body=%s", result.Code, beforeRevision, afterRevision, usageCalls.Load(), result.Body.String())
			}
			healthy.Store(true)
			result = refresh()
			after, err = f.accounts.GetQuotaWindows(ctx, []uint64{f.accountID})
			if err != nil {
				t.Fatal(err)
			}
			if result.Code != 200 || len(after[f.accountID]) != 3 || usageCalls.Load() != 2 {
				t.Fatalf("recovery status=%d windows=%+v calls=%d", result.Code, after[f.accountID], usageCalls.Load())
			}
			for _, window := range after[f.accountID] {
				if window.Remaining <= 0 {
					t.Fatalf("not refreshed: %+v", window)
				}
			}
			if f.generated.Load() != 0 {
				t.Fatal("quota refresh generated inference")
			}
		})
	}
}
