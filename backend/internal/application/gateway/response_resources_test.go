package gateway

import (
	"context"
	"errors"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

type resourceFaultStore struct {
	repository.ResponseRepository
	read, remove error
	beforeRemove func(context.Context)
}

func (r *resourceFaultStore) Get(ctx context.Context, id string, key uint64, now time.Time) (inferencedomain.ResponseOwnership, error) {
	if r.read != nil {
		return inferencedomain.ResponseOwnership{}, r.read
	}
	return r.ResponseRepository.Get(ctx, id, key, now)
}
func (r *resourceFaultStore) Delete(ctx context.Context, id string, key uint64) error {
	if r.beforeRemove != nil {
		r.beforeRemove(ctx)
	}
	if r.remove != nil {
		return r.remove
	}
	return r.ResponseRepository.Delete(ctx, id, key)
}

type ownedResourceAdapter struct {
	*scriptedBuildAdapter
	forward     func(context.Context, provider.ResponseResourceRequest) (*provider.Response, error)
	refreshErr  error
	unsupported bool
}

func (a *ownedResourceAdapter) ForwardResponse(ctx context.Context, r provider.ResponseResourceRequest) (*provider.Response, error) {
	return a.forward(ctx, r)
}
func (a *ownedResourceAdapter) RefreshCredential(ctx context.Context, c account.Credential) (provider.RefreshedCredential, error) {
	if a.refreshErr != nil {
		a.refreshes.Add(1)
		return provider.RefreshedCredential{}, a.refreshErr
	}
	return a.scriptedBuildAdapter.RefreshCredential(ctx, c)
}
func (a *ownedResourceAdapter) Definition() provider.Definition {
	d := a.scriptedBuildAdapter.Definition()
	d.Conversation.StoredResponses = !a.unsupported
	return d
}

type ownedResourceFixture struct {
	service     *Service
	store       *resourceFaultStore
	accountRepo *relational.AccountRepository
	credential  account.Credential
	key         clientkey.Key
	saved       inferencedomain.ResponseOwnership
	adapter     *ownedResourceAdapter
	concurrency repository.ConcurrencyLimiter
}

func newOwnedResourceFixture(t *testing.T, authType account.AuthType) *ownedResourceFixture {
	t.Helper()
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "resource.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts, models, audits := relational.NewAccountRepository(db), relational.NewModelRepository(db), relational.NewAuditRepository(db)
	refreshSecret := "refresh"
	if authType == account.AuthTypeSSO {
		refreshSecret = ""
	}
	c, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: authType, Name: "resource", SourceKey: "resource", EncryptedAccessToken: "old", EncryptedRefreshToken: refreshSecret, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = testsupport.Discover(ctx, models, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	if err = testsupport.Capabilities(ctx, models, accounts, c.ID, []string{"grok-4.6"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	route, err := models.GetByProviderUpstream(ctx, account.ProviderBuild, "grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	key, err := relational.NewClientKeyRepository(db).Create(ctx, clientkey.Key{ModelScope: clientkey.ModelScopeAll, Name: "resource", Prefix: "resource", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "synthetic", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	a := &ownedResourceAdapter{scriptedBuildAdapter: &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}}
	registry := providerimpl.NewRegistry(a)
	sticky := memory.NewStickyStore()
	concurrency := memory.NewConcurrencyLimiter()
	maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), security.RandomTokenSource{}, nil, nil, nil)
	store := &resourceFaultStore{ResponseRepository: relational.NewResponseRepository(db)}
	sel := selector.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(models, audits, maintenance, clientkeyapp.NewService("resource", nil, nil, nil, 120, 4, nil, security.RandomTokenSource{}), registry, sel, historyapp.NewResponseResources(store), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 2)
	now := time.Now().UTC()
	saved := inferencedomain.ResponseOwnership{ResponseID: "resource", AccountID: c.ID, ClientKeyID: key.ID, ModelRouteID: route.ID, Provider: c.Provider, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	if err = store.Save(ctx, saved); err != nil {
		t.Fatal(err)
	}
	return &ownedResourceFixture{service, store, accounts, c, key, saved, a, concurrency}
}
func (f *ownedResourceFixture) input() ResourceInput {
	return ResourceInput{ClientKey: f.key, ResponseID: f.saved.ResponseID}
}
func (f *ownedResourceFixture) noLease(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		n, err := f.concurrency.Current(context.Background(), repository.AccountConcurrencyKey(f.credential.ID))
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource leaked %d leases", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOwnedResourceReadRetainsStoreCause(t *testing.T) {
	for _, operation := range []string{"get", "delete", "previous"} {
		t.Run(operation, func(t *testing.T) {
			f := newOwnedResourceFixture(t, account.AuthTypeOAuth)
			cause := errors.New("private database read details")
			f.store.read = cause
			calls := 0
			f.adapter.forward = func(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
				calls++
				return nil, errors.New("unexpected send")
			}
			var result *Result
			var err error
			switch operation {
			case "get":
				result, err = f.service.GetResponse(context.Background(), f.input())
			case "delete":
				result, err = f.service.DeleteResponse(context.Background(), f.input())
			default:
				result, err = f.service.CreateResponse(context.Background(), Input{ClientKey: f.key, PublicModel: "grok-4.6", PreviousResponseID: f.saved.ResponseID, Body: []byte(`{"model":"grok-4.6","input":"next","previous_response_id":"resource"}`)})
			}
			if result != nil || !errors.Is(err, cause) || !errors.Is(err, historydomain.ErrResponseRead) || errors.Is(err, ErrResponseNotFound) || calls != 0 {
				t.Fatalf("result=%v err=%v calls=%d", result, err, calls)
			}
			t.Logf("saved=%#v key=%d", f.saved, f.key.ID)
			if _, err = f.store.ResponseRepository.Get(context.Background(), f.saved.ResponseID, f.key.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			f.noLease(t)
		})
	}
}

func TestOwnedResourceDeletionRequiresLocalCommitAndRecovers(t *testing.T) {
	for _, status := range []int{200, 404, 410, 0} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newOwnedResourceFixture(t, account.AuthTypeOAuth)
			cause := errors.New("private database deletion details")
			f.store.remove = cause
			calls := 0
			var bodies []*countedBody
			if status == 0 {
				f.adapter.unsupported = true
				f.service.providers = providerimpl.NewRegistry(f.adapter)
			}
			f.adapter.forward = func(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
				calls++
				b := &countedBody{Reader: strings.NewReader("private upstream body")}
				bodies = append(bodies, b)
				return &provider.Response{StatusCode: status, Header: make(http.Header), Body: b}, nil
			}
			result, err := f.service.DeleteResponse(context.Background(), f.input())
			if result != nil || !errors.Is(err, cause) || !errors.Is(err, historydomain.ErrResponseDelete) {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if _, err = f.store.ResponseRepository.Get(context.Background(), f.saved.ResponseID, f.key.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			for _, b := range bodies {
				if b.closed.Load() != 1 {
					t.Fatal("failed commit retained body")
				}
			}
			f.noLease(t)
			f.store.remove = nil
			result, err = f.service.DeleteResponse(context.Background(), f.input())
			if status == 200 {
				if err != nil || result == nil {
					t.Fatal(err)
				}
				_ = result.Body.Close()
				result.Finalize(Usage{}, "", "")
			} else if !errors.Is(err, ErrResponseNotFound) {
				t.Fatal(err)
			}
			if _, err = f.store.ResponseRepository.Get(context.Background(), f.saved.ResponseID, f.key.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("retry did not remove ownership: %v", err)
			}
			for _, b := range bodies {
				if b.closed.Load() != 1 {
					t.Fatal("body close count is not one")
				}
			}
			f.noLease(t)
			if status == 0 && calls != 0 || status != 0 && calls != 2 {
				t.Fatal(calls)
			}
		})
	}
}

func TestOwnedResourceAuthenticationRecoveryBudget(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		for _, scenario := range []string{"recover", "second_401", "refresh_failure", "permanent", "sso", "no_capacity", "second_transport_failure"} {
			t.Run(method+"/"+scenario, func(t *testing.T) {
				authType := account.AuthTypeOAuth
				if scenario == "sso" {
					authType = account.AuthTypeSSO
				}
				f := newOwnedResourceFixture(t, authType)
				cause := errors.New("synthetic unavailable")
				if scenario == "refresh_failure" {
					f.adapter.refreshErr = cause
				}
				if scenario == "permanent" {
					if _, err := f.accountRepo.ApplyCredential(context.Background(), f.credential.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRefreshFailed, Failure: account.CredentialRefreshFailure{Status: 400, Code: "invalid_grant", Permanent: true}}); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				var bodies []*countedBody
				f.adapter.forward = func(ctx context.Context, r provider.ResponseResourceRequest) (*provider.Response, error) {
					ctx = attemptmeta.Begin(ctx, attemptmeta.Path{})
					if err := infraegress.BeginDirectPhysicalCall(ctx); err != nil {
						return nil, err
					}
					calls++
					if r.Credential.ID != f.credential.ID || r.Method != method {
						t.Fatal("resource changed owner/method")
					}
					if calls == 2 && r.Credential.EncryptedAccessToken != "access-new" {
						t.Fatal("retry used stale credential")
					}
					if scenario == "no_capacity" {
						extra := attemptmeta.Begin(ctx, attemptmeta.Path{})
						if err := infraegress.BeginDirectPhysicalCall(extra); err != nil {
							t.Fatal(err)
						}
						calls++
					}
					status := 200
					if calls == 1 || scenario == "second_401" || scenario == "no_capacity" {
						status = 401
					}
					b := &countedBody{Reader: strings.NewReader("body")}
					bodies = append(bodies, b)
					response := &provider.Response{StatusCode: status, Header: make(http.Header), Body: b}
					infraegress.RecordDirectPhysicalCall(ctx, &http.Response{StatusCode: status, Body: b}, nil)
					if scenario == "second_transport_failure" && calls == 2 {
						return response, cause
					}
					return response, nil
				}
				var result *Result
				var err error
				if method == http.MethodGet {
					result, err = f.service.GetResponse(context.Background(), f.input())
				} else {
					result, err = f.service.DeleteResponse(context.Background(), f.input())
				}
				if scenario == "recover" {
					if err != nil || result == nil {
						t.Fatal(err)
					}
					if _, err = io.Copy(io.Discard, result.Body); err != nil {
						t.Fatal(err)
					}
					result.Finalize(Usage{}, "", "")
					_ = result.Body.Close()
				} else if result != nil || err == nil {
					t.Fatalf("scenario unexpectedly succeeded: %v %v", result, err)
				}
				wantCalls, wantRefresh := 2, int64(1)
				switch scenario {
				case "refresh_failure":
					wantCalls = 1
				case "permanent", "sso":
					wantCalls, wantRefresh = 1, 0
				case "no_capacity":
					wantRefresh = 0
					if !errors.Is(err, inferencedomain.ErrAttemptBudget) {
						t.Fatal(err)
					}
				}
				if calls != wantCalls || f.adapter.refreshes.Load() != wantRefresh {
					t.Fatalf("calls=%d refreshes=%d", calls, f.adapter.refreshes.Load())
				}
				for _, b := range bodies {
					if b.closed.Load() != 1 {
						t.Fatalf("body closes=%d", b.closed.Load())
					}
				}
				f.noLease(t)
			})
		}
	}
}

func TestOwnedResourceCancellationAndReturnedBodyFailure(t *testing.T) {
	for _, stage := range []string{"body_and_error", "cancel_after_return", "cancel_after_delete", "concurrent_delete"} {
		t.Run(stage, func(t *testing.T) {
			f := newOwnedResourceFixture(t, account.AuthTypeOAuth)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &countedBody{Reader: strings.NewReader("answer")}
			cause := errors.New("transport body failure")
			f.adapter.forward = func(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
				r := &provider.Response{StatusCode: 200, Header: make(http.Header), Body: body}
				if stage == "body_and_error" {
					return r, cause
				}
				if stage == "cancel_after_delete" {
					cancel()
				}
				if stage == "concurrent_delete" {
					if err := f.store.ResponseRepository.Delete(ctx, f.saved.ResponseID, f.key.ID); err != nil {
						t.Fatal(err)
					}
				}
				return r, nil
			}
			var result *Result
			var err error
			if stage == "cancel_after_delete" || stage == "concurrent_delete" {
				f.store.beforeRemove = func(commitCtx context.Context) {
					if commitCtx.Err() != nil {
						t.Fatal("known deletion inherited cancellation")
					}
				}
				result, err = f.service.DeleteResponse(ctx, f.input())
			} else {
				result, err = f.service.GetResponse(ctx, f.input())
			}
			if stage == "body_and_error" {
				if result != nil || !errors.Is(err, cause) {
					t.Fatal(err)
				}
			} else {
				if err != nil || result == nil {
					t.Fatal(err)
				}
				if stage == "cancel_after_return" {
					cancel()
					f.noLease(t)
					deadline := time.Now().Add(time.Second)
					for body.closed.Load() == 0 && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
				}
				_ = result.Body.Close()
				result.Finalize(Usage{}, "", "")
			}
			if body.closed.Load() != 1 {
				t.Fatal(body.closed.Load())
			}
			f.noLease(t)
			if stage == "cancel_after_delete" || stage == "concurrent_delete" {
				if _, err = f.store.ResponseRepository.Get(context.Background(), f.saved.ResponseID, f.key.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
					t.Fatal(err)
				}
			}
		})
	}
}
