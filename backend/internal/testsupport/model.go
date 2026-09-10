package testsupport

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Discover builds a fixture through the same M05 publication policy as account
// synchronization; SQL receives explicit route decisions, never model strings.
func Discover(ctx context.Context, models repository.ModelRepository, provider account.Provider, upstreamModels []string) error {
	routes, err := model.DiscoveredRoutes(provider, upstreamModels)
	if err != nil {
		return err
	}
	return models.MergeRoutes(ctx, provider, routes)
}

// Routes seeds explicit managed fixture routes. Each route uses the real
// namespace writer and constraints; production publication belongs to M05.
func Routes(ctx context.Context, models repository.ModelRepository, values []model.Route) error {
	byProvider := make(map[account.Provider][]model.Route)
	for _, value := range values {
		byProvider[value.Provider] = append(byProvider[value.Provider], value)
	}
	for provider, routes := range byProvider {
		if err := models.MergeRoutes(ctx, provider, routes); err != nil {
			return err
		}
	}
	return nil
}

// Capabilities seeds a successful observation through the production claim and
// completion contract. It cannot bypass current material or revision checks.
func Capabilities(ctx context.Context, models repository.ModelRepository, accounts repository.AccountRepository, id uint64, values []string, at time.Time) error {
	ref, err := models.BeginAccountCapabilitySync(ctx, id, at)
	if err != nil {
		return err
	}
	credential, err := accounts.Get(ctx, id)
	if err != nil {
		return err
	}
	return models.CompleteAccountCapabilitySync(ctx, ref, model.CapabilitySyncResult{Credential: credential.CredentialRef(), Models: values})
}
