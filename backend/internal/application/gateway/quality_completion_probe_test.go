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
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestLegacyProbeCleanRequiresCompletedResponse(t *testing.T) {
	thinking := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\n"
	completed := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"probe\",\"status\":\"completed\"}}\n\n"
	for _, test := range []struct {
		name, tail string
		readErr    bool
		want       qualitymodel.MeasurementOutcome
	}{
		{"completed", completed, false, qualitymodel.MeasurementClean},
		{"thinking_only", "", false, qualitymodel.MeasurementError},
		{"thinking_then_reset", "", true, qualitymodel.MeasurementError},
		{"completed_then_reset", completed, true, qualitymodel.MeasurementError},
		{"done_without_completed", "data: [DONE]\n\n", false, qualitymodel.MeasurementError},
		{"failed", "data: {\"type\":\"response.failed\"}\n\n", false, qualitymodel.MeasurementError},
		{"incomplete", "data: {\"type\":\"response.incomplete\"}\n\n", false, qualitymodel.MeasurementError},
		{"completed_failed_status", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n", false, qualitymodel.MeasurementError},
		{"completion_overflow", strings.Repeat(" ", qualityProbeCompletionBytes) + completed, false, qualitymodel.MeasurementError},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &countedBody{Reader: strings.NewReader(thinking + test.tail)}
			if test.readErr {
				body.Reader = io.MultiReader(body.Reader, guardReadFailure{})
			}
			identity := attemptmeta.Identity{ID: "fictional-probe", Provider: "grok_build", Model: "grok-4.6", RuleVersion: "fictional-rule", Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "low"}}
			spec := qualitymodel.ProbeExperiment{Version: qualitymodel.LegacyProbeExperimentVersion, TriggerEventID: "fictional-event", Baseline: identity, Sample: "inventory"}
			identity.Profile = spec.Profile()
			source := &changingGuardSource{}
			source.value.Store(&GuardSnapshot{Runtime: normalizeQualityRetry(QualityRetryRuntime{RuleVersion: identity.RuleVersion}), Kernel: builtinQualityKernel{}})
			s := newProbeTestService(providerimpl.NewRegistry(identifiedProbeAdapter{qualityProbeAttemptAdapter{body: func() io.ReadCloser { return body }}, identity}))
			s.SetGuardSnapshotSource(source)
			outcome, reason := probeOutcome(s, qualitymodel.WithProbeExperiment(context.Background(), spec), provider.ResponseResourceRequest{Credential: account.Credential{Provider: account.ProviderBuild}}, QualityRetryRuntime{})
			if outcome != test.want || body.closed.Load() != 1 {
				t.Fatalf("outcome=%s reason=%s closes=%d", outcome, reason, body.closed.Load())
			}
		})
	}
}

func TestProbeThinkingEvidenceStopsAndClosesUnreadTail(t *testing.T) {
	idle := newIdleQualityProbeBody()
	body := &replayReadCloser{Reader: io.MultiReader(strings.NewReader("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\n"), idle), source: idle}
	s := newProbeTestService(providerimpl.NewRegistry(qualityProbeAttemptAdapter{body: func() io.ReadCloser { return body }}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	outcome, reason := probeOutcome(s, ctx, provider.ResponseResourceRequest{Credential: account.Credential{Provider: account.ProviderBuild}}, QualityRetryRuntime{})
	if outcome != qualitymodel.MeasurementClean || ctx.Err() != nil {
		t.Fatalf("outcome=%s reason=%s", outcome, reason)
	}
	select {
	case <-idle.released:
	default:
		t.Fatal("probe left the raw reader blocked")
	}
}

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
	// Admission must stop before assembling the later completion event.
	raw := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\ndata: {\"type\":\n" +
		"data: \"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	stream := responseflow.New(io.NopCloser(strings.NewReader(raw)), responsebuffer.FromContext(ctx))
	stream.Observe(func(*responseflow.Event) { a.observed.Add(1) })
	return &provider.Response{StatusCode: 200, Body: stream, Attempt: identity}, nil
}

type probeEpochResolver struct{ epoch *atomic.Uint64 }

func (r probeEpochResolver) PathVersion(uint64) (uint64, bool) { return r.epoch.Load(), true }

func TestProbeFreezesPhysicalPolicyAndStopsCanonicalStreamAtEvidence(t *testing.T) {
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
	if observed.Load() != 1 || pool.Snapshot().Used != 0 {
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

func TestProbeSignalClassificationDoesNotWaitForAnswer(t *testing.T) {
	thinking := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\n"
	for _, tc := range []struct {
		name, raw string
		want      qualitymodel.MeasurementOutcome
	}{
		{"thinking_only", thinking, qualitymodel.MeasurementClean},
		{"answer_not_checked", thinking + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fictional wrong answer\"}\n\n", qualitymodel.MeasurementClean},
		{"later_failure_not_consumed", thinking + "data: {\"type\":\"response.failed\"}\n\n", qualitymodel.MeasurementClean},
		{"blank_thinking_is_not_evidence", "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\" \"}\n\n", qualitymodel.MeasurementError},
		{"usage_is_not_evidence", "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":128,\"output_tokens_details\":{\"reasoning_tokens\":128}}}}\n\n", qualitymodel.MeasurementError},
		{"failure_before_evidence", "data: {\"type\":\"response.failed\"}\n\n", qualitymodel.MeasurementError},
		{"text_without_thinking", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n", qualitymodel.MeasurementDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &countedBody{Reader: strings.NewReader(tc.raw)}
			s := newProbeTestService(providerimpl.NewRegistry(qualityProbeAttemptAdapter{body: func() io.ReadCloser { return body }}))
			outcome, reason := probeOutcome(s, context.Background(), provider.ResponseResourceRequest{Credential: account.Credential{Provider: account.ProviderBuild}}, QualityRetryRuntime{})
			if outcome != tc.want || body.closed.Load() != 1 {
				t.Fatalf("outcome=%s reason=%s closes=%d", outcome, reason, body.closed.Load())
			}
		})
	}
}
