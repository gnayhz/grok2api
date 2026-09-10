package model

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func modelManagementPair(t *testing.T) (*Service, *Service) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "management.db")
	open := func() *relational.Database {
		db, err := relational.OpenSQLite(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	a := open()
	if err := a.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	b := open()
	registry := provider.NewRegistry(&modelRouteAdapter{modelCapabilityAdapter: &modelCapabilityAdapter{}})
	return NewService(relational.NewModelRepository(a), relational.NewAccountRepository(a), nil, registry), NewService(relational.NewModelRepository(b), relational.NewAccountRepository(b), nil, registry)
}

func TestModelBindingSelectsAccountsOutsideFirstPage(t *testing.T) {
	s, _ := modelManagementPair(t)
	ctx := context.Background()
	var oldest uint64
	for index := range 1001 {
		v, _, err := s.accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: fmt.Sprintf("account-%04d", index), SourceKey: fmt.Sprintf("account-%04d", index), EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			oldest = v.ID
		}
	}
	if _, err := s.Create(ctx, CreateInput{PublicID: "old-account", Provider: account.ProviderBuild, UpstreamModel: "upstream", Capability: modeldomain.CapabilityResponses, Enabled: true, AccountIDs: []uint64{oldest}}); err != nil {
		t.Errorf("valid account outside first page rejected: %v", err)
	}
	for _, page := range []int{1, 2} {
		options, total, err := s.ListBindableAccounts(ctx, account.ProviderBuild, page, 1000, "")
		if err != nil {
			t.Fatal(err)
		}
		want := 1000
		if page == 2 {
			want = 1
		}
		if total != 1001 || len(options) != want {
			t.Fatalf("page %d: got %d total %d", page, len(options), total)
		}
		if page == 2 && options[0].ID != oldest {
			t.Fatalf("last page account = %d, want %d", options[0].ID, oldest)
		}
	}
	for _, search := range []string{"account-0000", fmt.Sprintf("#%d", oldest)} {
		options, total, err := s.ListBindableAccounts(ctx, account.ProviderBuild, 1, 20, search)
		if err != nil || total != 1 || len(options) != 1 || options[0].ID != oldest {
			t.Fatalf("search %q: %v total %d err %v", search, options, total, err)
		}
	}
}

type heldModelRead struct {
	repository.ModelRepository
	entered, release chan struct{}
	once             sync.Once
}

func (r *heldModelRead) Get(ctx context.Context, id uint64) (modeldomain.Route, error) {
	v, err := r.ModelRepository.Get(ctx, id)
	r.once.Do(func() {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	})
	return v, err
}

func TestModelPartialUpdateKeepsConcurrentOmittedFields(t *testing.T) {
	for _, lateRename := range []bool{true, false} {
		t.Run(fmt.Sprintf("late_rename_%t", lateRename), func(t *testing.T) {
			a, b := modelManagementPair(t)
			ctx := context.Background()
			created, err := a.Create(ctx, CreateInput{PublicID: "original", Provider: account.ProviderBuild, UpstreamModel: "upstream", Capability: modeldomain.CapabilityResponses, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			held := &heldModelRead{ModelRepository: a.models, entered: make(chan struct{}), release: make(chan struct{})}
			a.models = held
			var release sync.Once
			defer release.Do(func() { close(held.release) })
			name, enabled := "renamed", false
			older, newer := UpdateInput{PublicID: &name}, UpdateInput{Enabled: &enabled}
			if !lateRename {
				older, newer = newer, older
			}
			done := make(chan error, 1)
			go func() { _, err := a.Update(ctx, created.ID, older); done <- err }()
			select {
			case <-held.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("older update did not read")
			}
			if _, err := b.Update(ctx, created.ID, newer); err != nil {
				t.Fatal(err)
			}
			release.Do(func() { close(held.release) })
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("older update did not finish")
			}
			current, err := b.Get(ctx, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Enabled || modeldomain.ExternalPublicID(current.Provider, current.PublicID) != name {
				t.Errorf("partial update overwrote another edit: enabled=%t publicID=%s", current.Enabled, current.PublicID)
			}
		})
	}
}

func TestModelBindingScopeValidationAndUnchangedFields(t *testing.T) {
	s, _ := modelManagementPair(t)
	ctx := context.Background()
	build, _, err := s.accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "build", SourceKey: "build", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	web, _, err := s.accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "web", SourceKey: "web", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(ctx, CreateInput{PublicID: "scoped", Provider: account.ProviderBuild, UpstreamModel: "upstream", Capability: modeldomain.CapabilityResponses, Enabled: true, AccountIDs: []uint64{build.ID, build.ID}})
	if err != nil || len(created.BoundAccountIDs) != 1 {
		t.Fatalf("deduplicated binding: %+v %v", created, err)
	}
	enabled := false
	for _, ids := range [][]uint64{{0}, {web.ID}, {build.ID, web.ID}, {build.ID + 90000}, make([]uint64, 1001)} {
		if _, err := s.Update(ctx, created.ID, UpdateInput{Enabled: &enabled, AccountIDs: &ids}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("binding %v: %v", ids, err)
		}
		got, err := s.models.Get(ctx, created.ID)
		if err != nil || !got.Enabled || len(got.BoundAccountIDs) != 1 || got.BoundAccountIDs[0] != build.ID {
			t.Fatalf("invalid request changed fields: %+v %v", got, err)
		}
	}
}
