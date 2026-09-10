package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type failingPhysicalRecorder struct{ physicalEventRecorder }

func (*failingPhysicalRecorder) RecordPhysicalEvents(context.Context, []attemptmeta.PhysicalFact) error {
	return errors.New("injected persistence failure")
}

type probeFailureReader struct{ err error }

func (r probeFailureReader) Read([]byte) (int, error) { return 0, r.err }

// Exercise the actual producer, including physical fact persistence after a
// successful stream. Looking only at the app mapper would miss lost provenance.
func TestProbeFailureProvenanceAtMeasurementBoundary(t *testing.T) {
	thinking := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\n"
	completed := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	for _, test := range []struct {
		name           string
		forward, read  error
		status         int
		tail           string
		persistFailure bool
		want           model.ProbeFailure
	}{
		{name: "adapter capacity", forward: responsebuffer.ErrExhausted, want: model.ProbeFailureResource},
		{name: "adapter unknown HTTP text", forward: errors.New("HTTP evidence timed out"), want: model.ProbeFailureForward},
		{name: "admission capacity", read: responsebuffer.ErrExhausted, want: model.ProbeFailureResource},
		{name: "admission unknown evidence text", read: errors.New("evidence_timeout"), want: model.ProbeFailureAdmission},
		{name: "completion limit", tail: strings.Repeat(" ", qualityProbeCompletionBytes) + completed, want: model.ProbeFailureResource},
		{name: "persistence after success", tail: completed, persistFailure: true, want: model.ProbeFailurePersistence},
		{name: "upstream server", status: 503, want: model.ProbeFailureHTTPServer},
		{name: "upstream credential", status: 401, want: model.ProbeFailureHTTPRejected},
		{name: "upstream quota", status: 429, want: model.ProbeFailureHTTPRejected},
		{name: "empty stream", want: model.ProbeFailureEmptyStream},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Service{}
			s.providers = provider.NewRegistry(resourceTestAdapter{&scriptedBuildAdapter{}, func(ctx context.Context, _ provider.ResponseResourceRequest) (*provider.Response, error) {
				if test.forward != nil {
					return nil, test.forward
				}
				var body io.Reader = strings.NewReader("")
				if test.tail != "" {
					body = strings.NewReader(thinking + test.tail)
				}
				if test.read != nil {
					body = probeFailureReader{test.read}
				}
				status := test.status
				if status == 0 {
					status = 200
				}
				ctx = attemptmeta.Begin(ctx, attemptmeta.Path{})
				if err := infraegress.BeginDirectPhysicalCall(ctx); err != nil {
					return nil, err
				}
				raw := &http.Response{StatusCode: status, Body: io.NopCloser(body)}
				infraegress.RecordDirectPhysicalCall(ctx, raw, nil)
				return &provider.Response{StatusCode: status, Body: raw.Body, Attempt: attemptmeta.FromContext(ctx)}, nil
			}})
			if test.persistFailure {
				s.SetQualityEventRecorder(&failingPhysicalRecorder{})
			}
			got := s.qualityProbeMeasurement(context.Background(), provider.ResponseResourceRequest{Credential: account.Credential{Provider: account.ProviderBuild}}, QualityRetryRuntime{})
			if got.Outcome != model.MeasurementError || got.Failure != test.want {
				t.Fatalf("got %+v, want %s", got, test.want)
			}
		})
	}
}

func TestProbeCompletionChildBudgetCannotBecomeAvailabilityEvidence(t *testing.T) {
	kind := probeOperationFailure(context.Background(), context.DeadlineExceeded, model.ProbeFailureCompletion)
	if kind != model.ProbeFailureCompletionBudget || kind.SupportsAvailability() {
		t.Fatal(kind)
	}
}
