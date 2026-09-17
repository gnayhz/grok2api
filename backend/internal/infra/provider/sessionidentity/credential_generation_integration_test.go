package sessionidentity_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestSSOHTTPCompletionUsesObservedGeneration(t *testing.T) {
	for _, success := range []bool{false, true} {
		for _, kind := range []accountdomain.Provider{accountdomain.ProviderWeb, accountdomain.ProviderConsole} {
			for _, reimport := range []bool{false, true} {
				t.Run(fmt.Sprintf("success=%t/", success)+string(kind)+map[bool]string{true: "/reimport", false: "/current"}[reimport], func(t *testing.T) {
					ctx := context.Background()
					db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sso.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if err := db.InitializeSchema(ctx); err != nil {
						t.Fatal(err)
					}
					cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
					if err != nil {
						t.Fatal(err)
					}
					old, err := cipher.Encrypt("old-sso")
					if err != nil {
						t.Fatal(err)
					}
					fresh, err := cipher.Encrypt("new-sso")
					if err != nil {
						t.Fatal(err)
					}
					repo := relational.NewAccountRepository(db)
					original, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: kind, AuthType: accountdomain.AuthTypeSSO, Name: "sso", SourceKey: "sso", EncryptedAccessToken: old, AuthStatus: accountdomain.AuthStatusActive, FailureCount: 2, LastError: accountdomain.LastErrorMissingThinking})
					if err != nil {
						t.Fatal(err)
					}
					const oldUID = "11111111-1111-4111-8111-111111111111"
					const newUID = "22222222-2222-4222-8222-222222222222"
					peerKind, peerAuth := accountdomain.ProviderBuild, accountdomain.AuthTypeOAuth
					if kind == accountdomain.ProviderConsole {
						peerKind, peerAuth = accountdomain.ProviderWeb, accountdomain.AuthTypeSSO
					}
					peer, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: peerKind, AuthType: peerAuth, Name: "peer", SourceKey: "peer", UserID: oldUID, EncryptedAccessToken: old, AuthStatus: accountdomain.AuthStatusActive})
					if err != nil {
						t.Fatal(err)
					}
					entered, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					finish := func() { once.Do(func() { close(release) }) }
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if !strings.Contains(r.Header.Get("Cookie"), "old-sso") {
							t.Error("wrong SSO material sent")
						}
						close(entered)
						<-release
						if success {
							w.Header().Set("Content-Type", "application/json")
							_, _ = fmt.Fprintf(w, `{"user":{"id":%q,"email":"old@example.test","teamId":"old-team"}}`, oldUID)
						} else {
							w.WriteHeader(http.StatusUnauthorized)
						}
					}))
					defer func() { finish(); server.Close() }()
					egress := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
					defer egress.Close(ctx)
					var adapter provider.Adapter
					if kind == accountdomain.ProviderConsole {
						adapter = console.NewAdapter(console.Config{SessionBaseURL: server.URL}, egress, cipher, nil)
					} else {
						adapter = web.NewAdapter(web.Config{BaseURL: server.URL}, egress, cipher, nil, nil)
					}
					service := accountapp.NewService(repo, nil, nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
					result := make(chan error, 1)
					go func() { result <- service.SyncAccountIdentity(ctx, original.ID) }()
					select {
					case <-entered:
					case earlyErr := <-result:
						t.Fatalf("SSO request failed before I/O: %v", earlyErr)
					case <-time.After(3 * time.Second):
						t.Fatal("SSO request did not start")
					}
					if reimport {
						replacement := original
						replacement.EncryptedAccessToken = fresh
						if success {
							replacement.Email, replacement.UserID, replacement.TeamID = "new@example.test", newUID, "new-team"
						}
						if _, _, err := repo.UpsertByIdentity(ctx, replacement); err != nil {
							t.Fatal(err)
						}
					}
					finish()
					select {
					case err := <-result:
						if (!success && !errors.Is(err, provider.ErrUnauthorized)) || (success && err != nil) {
							t.Fatalf("rejected request lost error: %v", err)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("SSO completion did not stop")
					}
					stored, err := repo.Get(ctx, original.ID)
					if err != nil {
						t.Fatal(err)
					}
					if stored.FailureCount != 2 || stored.LastError != accountdomain.LastErrorMissingThinking {
						t.Fatal("auth diagnostic changed health")
					}
					if reimport {
						if stored.AuthStatus != accountdomain.AuthStatusActive || stored.AuthError != "" || stored.EncryptedAccessToken != fresh || stored.CredentialGeneration != original.CredentialGeneration+1 {
							t.Fatal("late SSO rejection poisoned new material")
						}
					} else if !success && (stored.AuthStatus != accountdomain.AuthStatusReauthRequired || stored.AuthError == "" || stored.ReauthMarkedAt == nil) {
						t.Fatal("current rejection did not persist auth failure")
					}
					linked := false
					for _, link := range stored.LinkedAccounts {
						if link.ID == peer.ID {
							linked = true
						}
					}
					if success && reimport {
						if stored.Email != "new@example.test" || stored.UserID != newUID || stored.TeamID != "new-team" || linked {
							t.Fatal("old successful HTTP response overwrote new identity or linked old peer")
						}
					} else if success {
						if stored.Email != "old@example.test" || stored.UserID != oldUID || stored.TeamID != "old-team" || !linked || stored.AuthStatus != accountdomain.AuthStatusActive {
							t.Fatal("current HTTP identity and trusted link did not commit")
						}
					} else if linked {
						t.Fatal("failed HTTP observation created a link")
					}

				})
			}
		}
	}
}
