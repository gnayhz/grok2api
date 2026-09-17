package accountsync_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

func TestInitialSyncImportHTTPJoinsCancellationAndKeepsCommittedImport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			entered, enter := initialGate()
			release, unblock := initialGate()
			f := newInitialFixture(t, dialect, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/billing" {
					enter()
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				_, _ = io.WriteString(w, "ok")
			})
			t.Cleanup(unblock)
			router := gin.New()
			accounthttp.NewHandler(accounthttp.Dependencies{Administration: f.maintenance, Credentials: f.maintenance, Maintenance: f.maintenance, Onboarding: accountsyncapp.NewOnboarding(f.maintenance, f.maintenance, f.service)}).Register(router.Group("/api/admin/v1"))
			handled := make(chan struct{}, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { router.ServeHTTP(w, r); handled <- struct{}{} }))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			request := func(ctx context.Context) (string, error) {
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				part, err := writer.CreateFormFile("files", "synthetic.json")
				if err != nil {
					return "", err
				}
				if _, err = io.WriteString(part, "http-import"); err != nil {
					return "", err
				}
				if err = writer.Close(); err != nil {
					return "", err
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/admin/v1/accounts/import", &body)
				if err != nil {
					return "", err
				}
				req.Header.Set("Content-Type", writer.FormDataContentType())
				res, err := server.Client().Do(req)
				if err != nil {
					return "", err
				}
				defer res.Body.Close()
				data, err := io.ReadAll(res.Body)
				if err != nil {
					return "", err
				}
				if res.StatusCode != 200 {
					return "", fmt.Errorf("HTTP %d: %s", res.StatusCode, data)
				}
				return string(data), nil
			}
			done := make(chan error, 1)
			go func() { _, err := request(ctx); done <- err }()
			awaitInitial(t, entered)
			cancel()
			if err := awaitInitial(t, done); err == nil {
				t.Fatal("canceled import client unexpectedly completed")
			}
			// The onboarding use case joins its initial-sync workers before returning.
			// No detached HTTP/SQL operation may outlive this handler.
			awaitInitial(t, handled)
			if state := f.pool.Snapshot(); state.Active != 0 || state.Queued != 0 {
				t.Fatalf("handler returned with pool work: %+v", state)
			}
			accounts, err := f.accounts.ListEnabled(context.Background(), accountdomain.ProviderBuild)
			if err != nil || len(accounts) != 1 {
				t.Fatalf("committed import missing: %d %v", len(accounts), err)
			}
			f.assertSnapshot(t, accounts[0].ID, false, false)
			unblock()
			data, err := request(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			awaitInitial(t, handled)
			if !strings.Contains(data, "event: complete\n") || !strings.Contains(data, `"synced":1`) || !strings.Contains(data, `"syncFailed":0`) {
				t.Fatalf("incomplete import stream: %s", data)
			}
			f.assertSnapshot(t, accounts[0].ID, true, true)
			if f.billingCalls.Load() != 2 || f.modelCalls.Load() != 1 {
				t.Fatalf("resume calls billing=%d models=%d", f.billingCalls.Load(), f.modelCalls.Load())
			}
		})
	}
}
