package model

import (
	"context"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// PublishCatalogs reconciles fixed public products at startup. A failure is
// returned to the application constructor; repeating it is safe after a partial
// run because each Provider publication is an idempotent namespace transaction.
func (s *Service) PublishCatalogs(ctx context.Context) error {
	for _, provider := range account.Providers() {
		routes := modeldomain.CatalogRoutes(provider)
		if len(routes) == 0 {
			continue
		}
		if err := s.models.ReplaceProviderRoutes(ctx, provider, routes); err != nil {
			return fmt.Errorf("初始化 %s 模型目录: %w", provider.ModelNamespace(), err)
		}
	}
	return nil
}

func (s *Service) publishDiscovered(ctx context.Context, provider account.Provider, upstreamModels []string) error {
	routes, err := modeldomain.DiscoveredRoutes(provider, upstreamModels)
	if err != nil {
		return err
	}
	return s.models.MergeRoutes(ctx, provider, routes)
}
