package gateway

import (
	"context"
	"errors"

	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

// QualityProbeAccounts resolves the same frozen model as the real measurement.
// Court owns quality/identity filtering; Selector owns ordinary account eligibility.
func (s *Service) QualityProbeAccounts(ctx context.Context, experiment qualitymodel.ProbeExperiment) ([]uint64, error) {
	if s.selector == nil {
		return nil, errors.New("probe selector unavailable")
	}
	if experiment.Version != "" {
		ctx = qualitymodel.WithProbeExperiment(ctx, experiment)
	}
	route, err := s.qualityProbeRoute(ctx)
	if err != nil {
		return nil, err
	}
	return s.selector.QualityProbeCandidates(ctx, route.Provider, route.ID, route.UpstreamModel, s.providers.QuotaMode(route.Provider, route.UpstreamModel))
}
