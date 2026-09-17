package cli

import (
	"context"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

func TestBuildDetectionCurrentOutcome(t *testing.T) {
	for _, scenario := range []string{"obsolete_rejection", "replacement_after_commit", "observer_failure", "selected_http", "all_http"} {
		t.Run(scenario, func(t *testing.T) {
			formalHTTP := strings.HasSuffix(scenario, "_http")
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "outcome.db"))
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
			encrypt := func(s string) string {
				t.Helper()
				out, err := cipher.Encrypt(s)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			var values []account.Credential
			count := 3
			if formalHTTP {
				count = 1
			}
			for i := range count {
				value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
					Name: fmt.Sprint(i), SourceKey: fmt.Sprint(i), EncryptedAccessToken: encrypt("old"), ExpiresAt: time.Now().Add(time.Hour), RefreshPermanent: true, AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				values = append(values, value)
			}
			var requests atomic.Int32
			replace := func() {
				replacement := values[0]
				var err error
				replacement.EncryptedAccessToken, err = cipher.Encrypt("new")
				if err == nil {
					_, _, err = relational.NewAccountRepository(db).UpsertByIdentity(ctx, replacement)
				}
				if err != nil {
					t.Error(err)
				}
			}
			if scenario == "replacement_after_commit" {
				var replaced atomic.Bool
				repo.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
					if event.Kind == repository.InvalidationAccountCredentialChanged && replaced.CompareAndSwap(false, true) {
						replace()
					}
				})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if scenario != "observer_failure" {
					if scenario == "obsolete_rejection" || formalHTTP {
						replace()
					}
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
			}))
			defer server.Close()
			adapter := NewAdapter(Config{BaseURL: server.URL}, cipher)
			adapter.http = server.Client()
			service := accountapp.NewService(repo, nil, nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
			service.SetDetectPool(batch.NewPool(1))
			ids := []uint64{values[0].ID}
			if scenario == "observer_failure" {
				ids = append(ids, values[1].ID, values[2].ID)
			}
			if formalHTTP {
				router := gin.New()
				accounthttp.NewHandler(accounthttp.Dependencies{Administration: service, Credentials: service, Maintenance: service, Onboarding: accountsyncapp.NewOnboarding(service, service, nil)}).Register(router.Group("/api/admin/v1"))
				body := fmt.Sprintf(`{"ids":["%d"]}`, values[0].ID)
				if scenario == "all_http" {
					body = `{"all":true}`
				}
				request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/accounts/detect", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				data := response.Body.String()
				if response.Code != 200 || !strings.Contains(data, `"succeeded":0,"failed":1`) || strings.Contains(data, `"outcome":"invalid"`) || requests.Load() != 1 {
					t.Fatalf("obsolete refusal HTTP response=%d calls=%d %s", response.Code, requests.Load(), data)
				}
				wantItems := 1
				if scenario == "all_http" {
					wantItems = 0
				}
				if strings.Count(data, "event: item\n") != wantItems {
					t.Fatalf("item filter: %s", data)
				}
				current, err := repo.Get(ctx, values[0].ID)
				if err != nil || current.AuthStatus != account.AuthStatusActive || current.CredentialGeneration != values[0].CredentialGeneration+1 {
					t.Fatalf("replacement auth/generation: %v", err)
				}
				return
			}
			observerErr := errors.New("consumer has disconnected")
			var items []accountapp.BuildDetectItemResult
			succeeded, _, runErr := service.DetectBuildAccountsWithProgress(ctx, ids, false, nil, func(item accountapp.BuildDetectItemResult) error {
				items = append(items, item)
				if scenario == "observer_failure" {
					return observerErr
				}
				return nil
			})
			if scenario != "observer_failure" {
				current, err := repo.Get(ctx, values[0].ID)
				if err != nil || current.AuthStatus != account.AuthStatusActive || current.CredentialGeneration != values[0].CredentialGeneration+1 {
					t.Fatalf("replacement fixture auth/gen incorrect: %v", err)
				}
				if runErr != nil || len(items) != 1 || items[0].Outcome != accountapp.BuildDetectOutcomeFailed {
					t.Fatalf("obsolete refusal reported persisted invalid although replacement remains active: items=%+v err=%v", items, runErr)
				}
			} else if !errors.Is(runErr, observerErr) || requests.Load() != 1 || len(items) != 1 || succeeded != 1 {
				t.Fatalf("observer failure not returned/stopped or changed generation count: succeeded=%d requests=%d observer_calls=%d err=%v", succeeded, requests.Load(), len(items), runErr)
			}
		})
	}
}

func TestBuildDetectionObserverFailureCancelsInflightAndReleasesPool(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "observer-cancel.db"))
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
	encrypted, err := cipher.Encrypt("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for i := range 10 {
		v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
			Name: fmt.Sprint(i), SourceKey: fmt.Sprint(i), EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
	}
	started, closed := make(chan struct{}, 10), make(chan struct{}, 10)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := requests.Add(1)
		started <- struct{}{}
		if call == 1 {
			// Exactly the initial three requests must be in flight when the
			// successful result reaches the failing downstream observer.
			for range 3 {
				select {
				case <-started:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"in_progress",`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		closed <- struct{}{}
	}))
	defer server.Close()
	adapter := NewAdapter(Config{BaseURL: server.URL}, cipher)
	adapter.http = server.Client()
	service := accountapp.NewService(repo, nil, nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
	pool := batch.NewPool(3)
	service.SetDetectPool(pool)
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	observerErr := errors.New("downstream unavailable")
	var observations atomic.Int32
	succeeded, _, err := service.DetectBuildAccountsWithProgress(callCtx, ids, false, nil, func(item accountapp.BuildDetectItemResult) error {
		observations.Add(1)
		if item.Outcome != accountapp.BuildDetectOutcomeOK {
			t.Error("first result was not completed")
		}
		return observerErr
	})
	if !errors.Is(err, observerErr) || succeeded != 1 || requests.Load() != 3 || observations.Load() != 1 {
		t.Fatalf("succeeded=%d requests=%d observations=%d err=%v", succeeded, requests.Load(), observations.Load(), err)
	}
	for range 2 {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("inflight response not closed")
		}
	}
	if snapshot := pool.Snapshot(); snapshot.Active != 0 || snapshot.Queued != 0 {
		t.Fatalf("pool still occupied: %+v", snapshot)
	}
	for _, id := range ids {
		current, err := repo.Get(ctx, id)
		if err != nil || current.AuthStatus != account.AuthStatusActive {
			t.Fatalf("cancellation damaged auth: %v", err)
		}
	}
}
