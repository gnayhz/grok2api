package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func (s *Service) prepareQualityProbe(ctx context.Context, accountID uint64) (provider.ResponseResourceRequest, func(), model.ProbeMeasurement) {
	empty := provider.ResponseResourceRequest{}
	route, err := s.qualityProbeRoute(ctx)
	if err != nil {
		return empty, nil, model.ProbeMeasurement{Outcome: model.MeasurementUnconfigured, Failure: model.ProbeFailureConfiguration, Reason: err.Error()}
	}
	view, err := s.accounts.Get(ctx, accountID)
	if err != nil {
		return empty, nil, model.ProbeMeasurement{Outcome: model.MeasurementError, Failure: model.ProbeFailureAccount, Reason: "load account: " + err.Error()}
	}
	if !qualityProbeOnBuildFace(view.Credential.Provider) {
		return empty, nil, model.ProbeMeasurement{Outcome: model.MeasurementError, Failure: model.ProbeFailureConfiguration, Reason: "cross-face probe rejected: build probe on non-build account"}
	}
	release, err := s.selector.AcquireProbeResources(ctx, accountID)
	if err != nil {
		return empty, nil, model.ProbeMeasurement{Outcome: model.MeasurementError, Failure: model.ProbeFailureCapacity, Reason: "probe resource budget: " + err.Error()}
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	fail := func(kind model.ProbeFailure, err error) (provider.ResponseResourceRequest, func(), model.ProbeMeasurement) {
		return empty, nil, model.ProbeMeasurement{Outcome: model.MeasurementError, Failure: kind, Reason: err.Error()}
	}
	credential, billing := view.Credential, view.Billing
	if s.selector != nil {
		lease, err := s.selector.AcquirePinnedForQualityProbe(ctx, route.Provider, accountID, route.ID, route.UpstreamModel, s.providers.QuotaMode(route.Provider, route.UpstreamModel), clientkey.AccountScope{})
		if err != nil {
			return fail(model.ProbeFailureCapacity, fmt.Errorf("probe account selection unavailable: %w", err))
		}
		if lease == nil {
			return fail(model.ProbeFailureCapacity, errors.New("probe account capacity unavailable"))
		}
		budgetRelease := release
		release = func() { lease.Release(); budgetRelease() }
		if lease.QuotaProbe {
			return fail(model.ProbeFailureCapacity, errors.New("probe quota recovery pending"))
		}
		credential, billing = lease.Credential, lease.Billing
	}
	credential, err = s.accounts.EnsureCredential(ctx, credential, false)
	if err != nil {
		return fail(model.ProbeFailureCredential, err)
	}
	request, err := s.qualityProbeRequestForContext(ctx, route, credential, billing)
	if err != nil {
		return fail(model.ProbeFailureExperiment, err)
	}
	handedOff = true
	return request, release, model.ProbeMeasurement{}
}
