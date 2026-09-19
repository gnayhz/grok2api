package gateway

import (
	"context"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type probeIdentityAdapter struct {
	qualityProbeAttemptAdapter
	source   *changingGuardSource
	epoch    *atomic.Uint64
	observed *atomic.Int64
}

func (a probeIdentityAdapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	ctx = attemptmeta.Begin(ctx, attemptmeta.Path{NodeID: 19})
	identity := attemptmeta.FromContext(ctx)
	a.epoch.Store(8)
	a.source.value.Store(&GuardSnapshot{Runtime: normalizeQualityRetry(QualityRetryRuntime{Enabled: true, Revision: 42, RuleVersion: "new", GuardedModels: []string{"grok-4.6"}}), Kernel: rejectAllKernel{}})
	// The proof must consume the completion event under the frozen policy.
	raw := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\ndata: {\"type\":\n" +
		"data: \"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	stream := responseflow.New(io.NopCloser(strings.NewReader(raw)), responsebuffer.FromContext(ctx))
	stream.Observe(func(*responseflow.Event) { a.observed.Add(1) })
	return &provider.Response{StatusCode: 200, Body: stream, Attempt: identity}, nil
}

type probeEpochResolver struct{ epoch *atomic.Uint64 }

func (r probeEpochResolver) PathVersion(uint64) (uint64, bool) { return r.epoch.Load(), true }

func TestProofFreezesPhysicalPolicyAndConsumesCompletedStream(t *testing.T) {
	epoch, observed := &atomic.Uint64{}, &atomic.Int64{}
	epoch.Store(7)
	source := &changingGuardSource{}
	source.value.Store(&GuardSnapshot{Runtime: normalizeQualityRetry(QualityRetryRuntime{Enabled: true, Revision: 41, RuleVersion: "original", GuardedModels: []string{"grok-4.6"}}), Kernel: builtinQualityKernel{}, PathResolver: probeEpochResolver{epoch}})
	s := newProbeTestService(providerimpl.NewRegistry(probeIdentityAdapter{source: source, epoch: epoch, observed: observed}))
	s.SetGuardSnapshotSource(source)
	pool := responsebuffer.NewPool(2 << 20)
	ctx := responsebuffer.WithContext(context.Background(), pool.Request(2<<20))
	ctx = qualitymodel.WithProbeExperiment(ctx, qualitymodel.NewProbeExperiment(qualitymodel.Observation{EventID: "fictional-event", Attempt: attemptmeta.Identity{
		ID: "fictional-trigger", Provider: "grok_build", Model: "grok-4.6", Revision: 41, RuleVersion: "original",
		Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "xhigh", Tools: true},
	}}))
	measurement := s.qualityProbeMeasurement(ctx, provider.ResponseResourceRequest{Credential: account.Credential{ID: 11, Provider: account.ProviderBuild}, Model: "grok-4.6"}, QualityRetryRuntime{})
	outcome, detail, identity := measurement.Outcome, measurement.Reason, measurement.Attempt
	if outcome != qualitymodel.MeasurementClean || identity.ID == "" || identity.AccountID != 11 || identity.Revision != 41 || identity.RuleVersion != "original" || identity.Path.Epoch != 7 || identity.Path.NodeID != 19 {
		t.Fatalf("outcome=%s detail=%s identity=%+v", outcome, detail, identity)
	}
	if observed.Load() != 2 || pool.Snapshot().Used != 0 {
		t.Fatalf("events=%d budget=%+v", observed.Load(), pool.Snapshot())
	}
}

// A normalized probe identity is supplied by the real adapter after it submits.
type identifiedProbeAdapter struct {
	qualityProbeAttemptAdapter
	identity attemptmeta.Identity
}

func (a identifiedProbeAdapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	response, err := a.qualityProbeAttemptAdapter.ForwardResponse(ctx, request)
	response.Attempt = a.identity
	return response, err
}
