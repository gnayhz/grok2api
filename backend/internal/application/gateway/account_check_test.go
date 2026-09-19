package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestAccountCheckDeadlineClosesThePhysicalReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	body := newIdleQualityProbeBody()
	ctx, resources := selector.NewAttemptResources(ctx)
	defer resources.Close()
	stream := resources.Own(responseflow.New(body, nil))
	done := make(chan struct{})
	var sample qualitymodel.AccountCheckSample
	var err error
	go func() {
		sample, err = readAccountCheckStream(ctx, stream, QualityRetryRuntime{}, resources)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		body.Close()
		<-done
		t.Fatal("account check left the physical reader blocked")
	}
	if !errors.Is(err, context.DeadlineExceeded) || sample.Outcome != qualitymodel.MeasurementError || sample.Completed {
		t.Fatalf("timeout became a finding: %+v %v", sample, err)
	}
	select {
	case <-body.released:
	default:
		t.Fatal("physical body not closed")
	}
}

func TestAccountCheckRecordsActualRuleWithoutTreatingUsageAsThinking(t *testing.T) {
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":120,\"output_tokens\":12}}}\n\n"
	for _, tc := range []struct {
		name, body string
		want       qualitymodel.MeasurementOutcome
	}{
		{"thinking", "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"fictional reasoning\"}\n\n" + terminal, qualitymodel.MeasurementClean},
		{"text_before_thinking", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" + terminal, qualitymodel.MeasurementDegraded},
		{"late_thinking_conflict", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"late fictional reasoning\"}\n\n" + terminal, qualitymodel.MeasurementError},
		{"usage_only", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":214,\"output_tokens\":500,\"output_tokens_details\":{\"reasoning_tokens\":499}}}}\n\n", qualitymodel.MeasurementDegraded},
		{"empty", "", qualitymodel.MeasurementError},
		{"missing_terminal", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n", qualitymodel.MeasurementError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := qualitymodel.ProbeExperiment{Version: qualitymodel.AccountCheckVersion, Sample: "brief-confirmation", Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "grok-4.6", RuleVersion: "fictional-rule"}}
			identity := spec.Baseline
			identity.ID, identity.AccountID, identity.Profile = "fictional-check/1", 7, spec.Profile()
			body := &countedBody{Reader: strings.NewReader(tc.body)}
			s := newProbeTestService(providerimpl.NewRegistry(identifiedProbeAdapter{qualityProbeAttemptAdapter{body: func() io.ReadCloser { return body }}, identity}))
			s.SetGuardSnapshotSource(StaticGuardSnapshotSource(QualityRetryRuntime{RuleVersion: identity.RuleVersion}))
			result := s.qualityProbeMeasurement(qualitymodel.WithProbeExperiment(context.Background(), spec), provider.ResponseResourceRequest{Credential: account.Credential{ID: 7, Provider: account.ProviderBuild}, Model: identity.Model}, QualityRetryRuntime{})
			if result.Outcome != tc.want || result.CheckEvidence == nil || body.closed.Load() != 1 {
				t.Fatalf("result=%+v closes=%d", result, body.closed.Load())
			}
			if result.CheckEvidence.Thinking != (tc.want == qualitymodel.MeasurementClean || tc.name == "late_thinking_conflict") {
				t.Fatalf("thinking evidence=%+v", result.CheckEvidence)
			}
			if tc.want == qualitymodel.MeasurementDegraded && result.CheckEvidence.Rule == "" {
				t.Fatal("missing actual rule")
			}
		})
	}
}

func TestResourceCheckStreamRejectsRefusalToolsAndIncompleteResponses(t *testing.T) {
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":123,\"output_tokens\":5}}}\n\n"
	text := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
	thinking := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"fictional thinking\"}\n\n"
	for _, tc := range []struct{ name, body, class string }{
		{"normal", thinking + text + terminal, "A"},
		{"no_thinking", text + terminal, "B"},
		{"empty", terminal, "unknown"},
		{"refusal", "data: {\"type\":\"response.refusal.delta\",\"delta\":\"fictional refusal\"}\n\n" + terminal, "unknown"},
		{"unexpected_tool", "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\"}}\n\n" + text + terminal, "unknown"},
		{"truncated", text, "unknown"},
		{"late_thinking", text + thinking + terminal, "conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, resources := selector.NewAttemptResources(context.Background())
			defer resources.Close()
			body := resources.Own(responseflow.New(io.NopCloser(strings.NewReader(tc.body)), nil))
			s, _ := readAccountCheckStream(ctx, body, QualityRetryRuntime{}, resources)
			s.Sample, s.IdentityVerified, s.PathVerified, s.PathFamily, s.PathKey = "token-short", true, true, 4, "fictional-path"
			s.Attempt = attemptmeta.Identity{ID: "fictional-probe", AccountID: 1, Path: attemptmeta.Path{NodeID: 2, Status: attemptmeta.PathRegistered}}
			if got := qualitymodel.ClassifyResourceSample(s); got != tc.class {
				t.Fatalf("got=%s want=%s sample=%+v", got, tc.class, s)
			}
		})
	}
}
