package account

import (
	"context"
	"errors"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"testing"
)

type conversionLinkPort struct {
	repository.AccountRepository
	after func()
}

func (p *conversionLinkPort) ImportAccounts(ctx context.Context, inputs []repository.AccountImport) ([]repository.AccountUpsertResult, error) {
	out, err := p.AccountRepository.ImportAccounts(ctx, inputs)
	if err == nil && p.after != nil {
		after := p.after
		p.after = nil
		after()
	}
	return out, err
}
func TestConversionAssociationBelongsToObservedSource(t *testing.T) {
	service, repo, _ := deviceCompletionService(t, &deviceCompletionAdapter{})
	ctx := context.Background()
	v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web", UserID: "old-user", EncryptedAccessToken: "old-material", AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	port := &conversionLinkPort{AccountRepository: repo, after: func() {
		replacement := v
		replacement.UserID = "new-user"
		replacement.EncryptedAccessToken = "new-material"
		if _, _, err := repo.UpsertByIdentity(ctx, replacement); err != nil {
			t.Fatal(err)
		}
	}}
	service.accounts = port
	service.providers = providerimpl.NewRegistry(&conversionCompletionAdapter{after: func() {}})
	service.refreshLock = memory.NewLockStore()
	buildID, _, _, callErr := service.convertWebAccountToBuild(ctx, v.ID, BuildConversionAll)
	current, err := repo.Get(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.CredentialGeneration != v.CredentialGeneration+1 || current.UserID != "new-user" {
		t.Fatal("replacement fixture did not commit")
	}
	if !errors.Is(callErr, ErrConflict) || buildID == 0 {
		t.Fatalf("association failure did not report retained material and conflict: id=%d err=%v", buildID, callErr)
	}
	if current.LinkedAccountID != 0 {
		t.Fatalf("old conversion linked account %d to new source generation %d, service_error=%v returned_build=%d", current.LinkedAccountID, current.CredentialGeneration, callErr, buildID)
	}
}
