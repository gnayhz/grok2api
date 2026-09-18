package app

import (
	"context"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

type resourceCheckPaths struct {
	manager  *infraegress.Manager
	settings *settingsapp.Service
}

func (p resourceCheckPaths) ProbeBuildTarget(ctx context.Context, credential account.Credential, nodeID uint64) (string, int, uint64, error) {
	return p.manager.ProbeBuildTarget(ctx, credential, nodeID, p.settings.Get().Config.ProviderBuild.BaseURL)
}
