package gateway

import (
	"context"
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
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestProbeCleanRequiresCompletedResponse(t *testing.T) {
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
			s := &Service{providers: provider.NewRegistry(qualityProbeAttemptAdapter{body: func() io.ReadCloser { return body }})}
			outcome, reason := s.qualityProbeAttempt(context.Background(), provider.ResponseResourceRequest{Credential: account.Credential{Provider: account.ProviderBuild}}, QualityRetryRuntime{})
			if outcome != test.want || body.closed.Load() != 1 {
				t.Fatalf("outcome=%s reason=%s closes=%d", outcome, reason, body.closed.Load())
			}
		})
	}
}

func TestProbeCompletionCancellationClosesPendingRead(t *testing.T) {
	idle := newIdleQualityProbeBody()
	body := &replayReadCloser{Reader: io.MultiReader(strings.NewReader("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\n"), idle), source: idle}
	s := &Service{providers: provider.NewRegistry(qualityProbeAttemptAdapter{body: func() io.ReadCloser { return body }})}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	outcome, reason := s.qualityProbeAttempt(ctx, provider.ResponseResourceRequest{Credential: account.Credential{Provider: account.ProviderBuild}}, QualityRetryRuntime{})
	if outcome != qualitymodel.MeasurementError || !strings.Contains(reason, "completion") {
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
	// Multi-line data must be interpreted once by admission and completion.
	raw := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\ndata: {\"type\":\n" +
		"data: \"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	stream := responseflow.New(io.NopCloser(strings.NewReader(raw)), responsebuffer.FromContext(ctx))
	stream.Observe(func(*responseflow.Event) { a.observed.Add(1) })
	return &provider.Response{StatusCode: 200, Body: stream, Attempt: identity}, nil
}

type probeEpochResolver struct{ epoch *atomic.Uint64 }

func (r probeEpochResolver) PathVersion(uint64) (uint64, bool) { return r.epoch.Load(), true }

func TestProbeFreezesPhysicalPolicyAndSharesCanonicalCompletion(t *testing.T) {
	epoch, observed := &atomic.Uint64{}, &atomic.Int64{}
	epoch.Store(7)
	source := &changingGuardSource{}
	source.value.Store(&GuardSnapshot{Runtime: normalizeQualityRetry(QualityRetryRuntime{Enabled: true, Revision: 41, RuleVersion: "original", GuardedModels: []string{"grok-4.6"}}), Kernel: builtinQualityKernel{}, PathResolver: probeEpochResolver{epoch}})
	s := &Service{providers: provider.NewRegistry(probeIdentityAdapter{source: source, epoch: epoch, observed: observed})}
	s.SetGuardSnapshotSource(source)
	pool := responsebuffer.NewPool(2 << 20)
	ctx := responsebuffer.WithContext(context.Background(), pool.Request(2<<20))
	measurement := s.qualityProbeMeasurement(ctx, provider.ResponseResourceRequest{Credential: account.Credential{ID: 11, Provider: account.ProviderBuild}, Model: "grok-4.6"}, QualityRetryRuntime{})
	outcome, detail, identity := measurement.Outcome, measurement.Reason, measurement.Attempt
	if outcome != qualitymodel.MeasurementClean || identity.ID == "" || identity.AccountID != 11 || identity.Revision != 41 || identity.RuleVersion != "original" || identity.Path.Epoch != 7 || identity.Path.NodeID != 19 {
		t.Fatalf("outcome=%s detail=%s identity=%+v", outcome, detail, identity)
	}
	if observed.Load() != 2 || pool.Snapshot().Used != 0 {
		t.Fatalf("events=%d budget=%+v", observed.Load(), pool.Snapshot())
	}
}
