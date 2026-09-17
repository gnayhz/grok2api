package gateway

import (
	"context"
	"errors"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type failingNameLookup struct {
	repository.ModelRepository
	err error
}

func (r failingNameLookup) GetByPublicIDCandidates(context.Context, string) ([]model.Route, error) {
	return nil, r.err
}

func TestConfiguredModelNameStopsReasoningAliasFallback(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "dynamic", true: "registered"}[fixed], func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "priority.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo, accounts := relational.NewModelRepository(db), relational.NewAccountRepository(db)
			kind, baseName, requested := account.ProviderBuild, "grok-4.5", "grok-4.5-low"
			if fixed {
				kind, baseName, requested = account.ProviderConsole, "grok-4.3", "grok-4.3-high"
			}
			credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: kind, Name: "alias", SourceKey: "alias", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			base, err := repo.Create(ctx, model.Route{PublicID: baseName, Provider: kind, UpstreamModel: baseName, Capability: model.CapabilityResponses, Enabled: true}, []uint64{credential.ID})
			if err != nil {
				t.Fatal(err)
			}
			named, err := repo.Create(ctx, model.Route{PublicID: requested, Provider: kind, UpstreamModel: "grok-4.5", Capability: model.CapabilityResponses, Enabled: false}, []uint64{credential.ID})
			if err != nil {
				t.Fatal(err)
			}
			service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), models: repo, providers: providerimpl.NewRegistry(console.NewAdapter(console.Config{}, nil, nil, nil))}
			for _, allow := range []bool{false, true} {
				if rows, effort, err := service.resolvePublicModelRoutes(ctx, requested, allow); !errors.Is(err, repository.ErrNotFound) {
					t.Errorf("disabled name redirected (allow=%t): %+v effort=%s err=%v", allow, rows, effort, err)
				}
			}
			if _, err := repo.UpdateManyEnabled(ctx, []uint64{named.ID}, true); err != nil {
				t.Fatal(err)
			}
			if rows, effort, err := service.resolvePublicModelRoutes(ctx, requested, true); err != nil || len(rows) != 1 || rows[0].ID != named.ID || effort != "" {
				t.Errorf("literal name lost priority: %+v %s %v", rows, effort, err)
			}
			// Keep the base available while only the literal's binding is offline.
			offline, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: kind, Name: "offline", SourceKey: "offline", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			no := false
			patch := repository.AccountAdminPatch{}
			patch.Enabled = &no
			if _, err := accounts.UpdateAdministration(ctx, offline.ID, patch); err != nil {
				t.Fatal(err)
			}
			ids := []uint64{offline.ID}
			if _, err := repo.Patch(ctx, named.ID, model.RoutePatch{AccountIDs: &ids}); err != nil {
				t.Fatal(err)
			}
			for _, allow := range []bool{false, true} {
				if rows, effort, err := service.resolvePublicModelRoutes(ctx, requested, allow); !errors.Is(err, ErrNoAvailableAccount) {
					t.Errorf("unavailable name redirected (allow=%t): %+v %s %v", allow, rows, effort, err)
				}
			}
			if err := repo.Delete(ctx, named.ID); err != nil {
				t.Fatal(err)
			}
			if rows, effort, err := service.resolvePublicModelRoutes(ctx, requested, true); err != nil || len(rows) != 1 || rows[0].ID != base.ID || effort == "" {
				t.Errorf("true absence no longer supports aliases: %+v %s %v", rows, effort, err)
			}
			fault := errors.New("name lookup unavailable")
			service.models = failingNameLookup{ModelRepository: repo, err: fault}
			if _, _, err := service.resolvePublicModelRoutes(ctx, requested, true); !errors.Is(err, fault) {
				t.Errorf("SQL failure was reinterpreted as a different alias: %v", err)
			}
		})
	}
}
