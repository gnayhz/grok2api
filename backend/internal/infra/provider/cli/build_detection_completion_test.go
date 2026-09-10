package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

func TestBuildDetectionRequiresCompletedReadableResponse(t *testing.T) {
	for _, scenario := range []string{"truncated_success_http", "failed_response_http_200", "completed_response", "empty_output", "reasoning_only", "malformed", "body_over_limit"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "detect.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			token, err := cipher.Encrypt("synthetic-access")
			if err != nil {
				t.Fatal(err)
			}
			value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "detect", SourceKey: "detect", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body := `{"id":"resp-detect","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
				if scenario == "failed_response_http_200" {
					body = `{"id":"resp-failed","object":"response","status":"failed","error":{"code":"server_error","message":"generation failed"},"output":[]}`
				}
				switch scenario {
				case "empty_output":
					body = `{"status":"completed","output":[]}`
				case "reasoning_only":
					body = `{"status":"completed","output":[{"type":"reasoning","summary":[]}]}`
				case "malformed":
					body = `{broken`
				case "body_over_limit":
					body += strings.Repeat(" ", provider.MaxDiagnosticBodyBytes)
				}
				if scenario == "truncated_success_http" {
					w.Header().Set("Content-Length", fmt.Sprint(len(body)+100))
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			adapter := NewAdapter(Config{BaseURL: upstream.URL}, cipher)
			adapter.http = upstream.Client()
			service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, provider.NewRegistry(adapter), cipher, nil)
			var items []accountapp.BuildDetectItemResult
			success, failed, err := service.DetectBuildAccountsWithProgress(ctx, []uint64{value.ID}, false, nil, func(v accountapp.BuildDetectItemResult) error { items = append(items, v); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || len(items) != 1 {
				t.Fatalf("fixture calls=%d items=%d", calls.Load(), len(items))
			}
			if scenario == "completed_response" {
				if success != 1 || failed != 0 || items[0].Outcome != accountapp.BuildDetectOutcomeOK {
					t.Fatalf("completed result failed: %d %d %+v", success, failed, items[0])
				}
				return
			}
			if success != 0 || failed != 1 || items[0].Outcome != accountapp.BuildDetectOutcomeFailed {
				t.Fatalf("uncompleted upstream was reported available: succeeded=%d failed=%d outcome=%s reason=%q", success, failed, items[0].Outcome, items[0].Reason)
			}
			current, err := repo.Get(ctx, value.ID)
			if err != nil || current.AuthStatus != account.AuthStatusActive {
				t.Fatalf("transport/generation failure invalidated credentials: %v", err)
			}
		})
	}
}

func TestBuildDetectionCancellationDoesNotReportAvailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "detect-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("synthetic-access")
	if err != nil {
		t.Fatal(err)
	}
	value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "cancel", SourceKey: "cancel", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	entered, closed := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"in_progress",`)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(closed)
	}))
	defer upstream.Close()
	adapter := NewAdapter(Config{BaseURL: upstream.URL}, cipher)
	adapter.http = upstream.Client()
	service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, provider.NewRegistry(adapter), cipher, nil)
	type outcome struct {
		success, failed int
		err             error
		items           []accountapp.BuildDetectItemResult
	}
	done := make(chan outcome, 1)
	go func() {
		var out outcome
		out.success, out.failed, out.err = service.DetectBuildAccountsWithProgress(ctx, []uint64{value.ID}, false, nil, func(item accountapp.BuildDetectItemResult) error { out.items = append(out.items, item); return nil })
		done <- out
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("upstream did not begin")
	}
	cancel()
	select {
	case out := <-done:
		if !errors.Is(out.err, context.Canceled) || out.success != 0 || out.failed != 1 || len(out.items) != 1 || out.items[0].Outcome != accountapp.BuildDetectOutcomeFailed {
			t.Fatalf("canceled detection: %+v", out)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled detection did not finish")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled body was not closed")
	}
	current, err := repo.Get(context.Background(), value.ID)
	if err != nil || current.AuthStatus != account.AuthStatusActive || current.HealthRevision != value.HealthRevision {
		t.Fatalf("cancellation changed account status: %v", err)
	}
}

func TestBuildDetectionFormalHTTPReportsActualCompletion(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint("all=", all), func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "detect-http.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			var ids []uint64
			for _, name := range []string{"completed", "failed", "invalid"} {
				encrypted, err := cipher.Encrypt(name)
				if err != nil {
					t.Fatal(err)
				}
				value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: name, SourceKey: name, EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), RefreshPermanent: true, AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, value.ID)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				switch token {
				case "completed":
					_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
				case "failed":
					_, _ = io.WriteString(w, `{"status":"failed","output":[],"error":{"code":"server_error"}}`)
				case "invalid":
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key"}}`)
				default:
					t.Error("unexpected account token")
					http.Error(w, "unknown", 500)
				}
			}))
			defer upstream.Close()
			adapter := NewAdapter(Config{BaseURL: upstream.URL}, cipher)
			adapter.http = upstream.Client()
			service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, provider.NewRegistry(adapter), cipher, nil)
			router := gin.New()
			accounthttp.NewHandler(service, nil).Register(router.Group("/api/admin/v1"))
			body := fmt.Sprintf(`{"ids":["%d","%d","%d"]}`, ids[0], ids[1], ids[2])
			if all {
				body = `{"all":true}`
			}
			request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/accounts/detect", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			data := response.Body.String()
			if response.Code != 200 || !strings.Contains(data, `"succeeded":1,"failed":2`) || !strings.Contains(data, `"completed":3,"total":3`) {
				t.Fatalf("completion summary: %d %s", response.Code, data)
			}
			itemCount := strings.Count(data, "event: item\n")
			wantItems := 3
			if all {
				wantItems = 1
			}
			if itemCount != wantItems || !strings.Contains(data, `"outcome":"invalid"`) {
				t.Fatalf("item scope=%d expected=%d %s", itemCount, wantItems, data)
			}
			if !all && (!strings.Contains(data, `"outcome":"failed"`) || !strings.Contains(data, `"outcome":"ok"`)) {
				t.Fatalf("selected outcomes missing: %s", data)
			}
			for i, id := range ids {
				current, err := repo.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				want := account.AuthStatusActive
				if i == 2 {
					want = account.AuthStatusReauthRequired
				}
				if current.AuthStatus != want {
					t.Fatalf("account %d auth=%s want=%s", i, current.AuthStatus, want)
				}
			}
		})
	}
}

func TestBuildDetectionRefreshChecksSecondResponseAndPersistsRotatedMaterial(t *testing.T) {
	for _, scenario := range []string{"completed", "failed", "truncated", "second_401", "state_write_failed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "detect-refresh.db")
			db, err := relational.OpenSQLite(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			encrypt := func(value string) string {
				t.Helper()
				out, err := cipher.Encrypt(value)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			value, _, err := repo.UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, SourceKey: "refresh", Name: "refresh",
				EncryptedAccessToken: encrypt("old-access"), EncryptedRefreshToken: encrypt("old-refresh"),
				ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "state_write_failed" {
				faultDB, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer faultDB.Close()
				if _, err := faultDB.ExecContext(ctx, `CREATE TRIGGER fail_detect_reauth BEFORE UPDATE OF auth_status ON provider_accounts WHEN NEW.auth_status = 'reauthRequired' BEGIN SELECT RAISE(ABORT, 'detect reauth write failed'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var calls, refreshes atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					refreshes.Add(1)
					if r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != "old-refresh" {
						t.Error("unexpected OAuth refresh request")
					}
					_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
					return
				}
				calls.Add(1)
				if r.Header.Get("Authorization") == "Bearer old-access" || scenario == "second_401" || scenario == "state_write_failed" {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key"}}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer fresh-access" {
					t.Error("retry did not use committed fresh access")
				}
				body := `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`
				if scenario == "failed" {
					body = `{"status":"failed","output":[],"error":{"code":"server_error"}}`
				}
				if scenario == "truncated" {
					w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
				}
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			adapter := NewAdapter(Config{BaseURL: upstream.URL}, cipher)
			adapter.http, adapter.oauth.http, adapter.oauth.tokenURL = upstream.Client(), upstream.Client(), upstream.URL+"/token"
			service := accountapp.NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
			var items []accountapp.BuildDetectItemResult
			succeeded, failed, err := service.DetectBuildAccountsWithProgress(ctx, []uint64{value.ID}, false, nil, func(item accountapp.BuildDetectItemResult) error { items = append(items, item); return nil })
			if err != nil || calls.Load() != 2 || refreshes.Load() != 1 || len(items) != 1 {
				t.Fatalf("calls=%d refreshes=%d items=%d err=%v", calls.Load(), refreshes.Load(), len(items), err)
			}
			wantOutcome, wantSucceeded := accountapp.BuildDetectOutcomeFailed, 0
			if scenario == "completed" {
				wantOutcome, wantSucceeded = accountapp.BuildDetectOutcomeOK, 1
			}
			if scenario == "second_401" {
				wantOutcome = accountapp.BuildDetectOutcomeInvalid
			}
			if items[0].Outcome != wantOutcome || succeeded != wantSucceeded || failed != 1-wantSucceeded {
				t.Fatalf("succeeded=%d failed=%d item=%+v", succeeded, failed, items[0])
			}
			current, err := repo.Get(ctx, value.ID)
			if err != nil {
				t.Fatal(err)
			}
			access, err := cipher.Decrypt(current.EncryptedAccessToken)
			if err != nil {
				t.Fatal(err)
			}
			refresh, err := cipher.Decrypt(current.EncryptedRefreshToken)
			if err != nil {
				t.Fatal(err)
			}
			wantAuth := account.AuthStatusActive
			if scenario == "second_401" {
				wantAuth = account.AuthStatusReauthRequired
			}
			if access != "fresh-access" || refresh != "fresh-refresh" || current.CredentialGeneration != value.CredentialGeneration+1 || current.AuthStatus != wantAuth || current.HealthRevision != value.HealthRevision {
				t.Fatalf("rotation/auth/health mismatch generation=%d auth=%s health=%d", current.CredentialGeneration, current.AuthStatus, current.HealthRevision)
			}
			if scenario == "state_write_failed" && !strings.Contains(items[0].Reason, "detect reauth write failed") {
				t.Fatalf("state failure hidden: %+v", items[0])
			}
		})
	}
}
