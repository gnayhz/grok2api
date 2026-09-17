package account

import (
	"bytes"
	"context"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

type deletionImportHTTPAdapter struct{ kind accountdomain.Provider }

func (a deletionImportHTTPAdapter) Provider() accountdomain.Provider { return a.kind }
func (a deletionImportHTTPAdapter) Definition() provider.Definition {
	auth := accountdomain.AuthTypeSSO
	if a.kind == accountdomain.ProviderBuild {
		auth = accountdomain.AuthTypeOAuth
	}
	return provider.Definition{Provider: a.kind, ModelNamespace: a.kind.ModelNamespace(), Credential: provider.CredentialSurface{AuthType: auth, Import: true}}
}
func (a deletionImportHTTPAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return []provider.CredentialSeed{{Provider: a.kind, AuthType: a.Definition().Credential.AuthType, Name: "HTTP import", SourceKey: "http-import", Email: "http@example.test", AccessToken: "synthetic"}}, nil
}
func (deletionImportHTTPAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}

type deletionImportHTTPPort struct {
	repository.AccountRepository
	afterRead func() error
}

func (p *deletionImportHTTPPort) TombstonedEmails(ctx context.Context, emails []string) (map[string]struct{}, error) {
	out, err := p.AccountRepository.TombstonedEmails(ctx, emails)
	if err == nil && p.afterRead != nil {
		fn := p.afterRead
		p.afterRead = nil
		err = fn()
	}
	return out, err
}

func TestAccountHTTPDeletionPreventsReimport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderWeb, accountdomain.ProviderConsole} {
		for _, entry := range []string{"single", "batch", "cleanup", "linked", "during_import"} {
			t.Run(string(kind)+"/"+entry, func(t *testing.T) {
				ctx := context.Background()
				db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "http-deletion.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				if err := db.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					t.Fatal(err)
				}
				repo := relational.NewAccountRepository(db)
				adapter := deletionImportHTTPAdapter{kind: kind}
				v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: kind, AuthType: adapter.Definition().Credential.AuthType, Name: "HTTP deleted", SourceKey: "http-deleted", Email: "http@example.test", EncryptedAccessToken: "synthetic"})
				if err != nil {
					t.Fatal(err)
				}
				port := &deletionImportHTTPPort{AccountRepository: repo}
				s := accountapp.NewService(port, nil, nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
				router := gin.New()
				newTestHandler(s, nil).Register(router.Group("/api/admin/v1"))
				server := httptest.NewServer(router)
				t.Cleanup(server.Close)
				path, method, body := fmt.Sprintf("/accounts/%d", v.ID), http.MethodDelete, ""
				if entry == "batch" {
					path = "/accounts"
					body = fmt.Sprintf(`{"provider":%q,"ids":["%d"]}`, kind, v.ID)
				} else if entry == "cleanup" {
					disabled := false
					if _, err := repo.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &disabled}}); err != nil {
						t.Fatal(err)
					}
					path, method = "/accounts/cleanup", http.MethodPost
					body = fmt.Sprintf(`{"provider":%q,"statuses":["disabled"]}`, kind)
				} else if entry == "linked" {
					// Delete through a Web root and import the selected peer's
					// different email, covering the former roots-only snapshot.
					if kind != accountdomain.ProviderWeb {
						web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "HTTP root", SourceKey: "root", Email: "root@example.test", EncryptedAccessToken: "synthetic"})
						if err != nil {
							t.Fatal(err)
						}
						if kind == accountdomain.ProviderBuild {
							err = repo.LinkWebToBuild(ctx, web.CredentialRef(), v.CredentialRef())
						} else {
							// Reconcile Console by the same explicit SSO digest.
							web.SourceKey = "sso:" + strings.Repeat("a", 64)
							v.SourceKey = "console-sso:" + strings.Repeat("a", 64)
							web, _, err = repo.UpsertByIdentity(ctx, web)
							if err == nil {
								v, _, err = repo.UpsertByIdentity(ctx, v)
							}
							if err == nil {
								err = repo.ReconcileProviderLinks(ctx, web.ID)
							}
						}
						if err != nil {
							t.Fatal(err)
						}
						path = fmt.Sprintf("/accounts/%d", web.ID)
						body = fmt.Sprintf(`{"provider":"grok_web","linkedDeleteTargets":[%q]}`, kind)
					}
				}
				deleteRequest := func() error {
					req, err := http.NewRequest(method, server.URL+"/api/admin/v1"+path, strings.NewReader(body))
					if err != nil {
						return err
					}
					req.Header.Set("Content-Type", "application/json")
					resp, err := server.Client().Do(req)
					if err != nil {
						return err
					}
					defer resp.Body.Close()
					data, err := io.ReadAll(resp.Body)
					if err != nil {
						return err
					}
					if resp.StatusCode != http.StatusOK {
						return fmt.Errorf("delete HTTP %d: %s", resp.StatusCode, data)
					}
					return nil
				}
				if entry == "during_import" {
					port.afterRead = deleteRequest
				} else if err := deleteRequest(); err != nil {
					t.Fatal(err)
				}
				var upload bytes.Buffer
				writer := multipart.NewWriter(&upload)
				part, err := writer.CreateFormFile("files", "synthetic.json")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write([]byte("synthetic")); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				endpoint := map[accountdomain.Provider]string{accountdomain.ProviderBuild: "import", accountdomain.ProviderWeb: "web/import", accountdomain.ProviderConsole: "console/import"}[kind]
				req, err := http.NewRequest(http.MethodPost, server.URL+"/api/admin/v1/accounts/"+endpoint, &upload)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", writer.FormDataContentType())
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "event: complete\n") || !strings.Contains(string(data), `"created":0,"updated":0,"skipped":1`) {
					t.Fatalf("HTTP reimport: status=%d, %s, %v", resp.StatusCode, data, err)
				}
				values, err := repo.ListEnabled(ctx, kind)
				if err != nil || len(values) != 0 {
					t.Fatalf("HTTP reimport left accounts: %d, %v", len(values), err)
				}
			})
		}
	}
}
