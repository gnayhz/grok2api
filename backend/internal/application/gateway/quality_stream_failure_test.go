package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

func TestQualityExplicitFailureIsNeverEmptyOrDegraded(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		for _, protocol := range []string{qualityProtocolResponses, qualityProtocolChat, qualityProtocolAnthropic} {
			for _, payload := range []string{
				`{"type":"error","error":{"code":"invalid-argument","message":"synthetic parameter failure"},"output":"must-not-be-audited"}`,
				`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"synthetic unavailable"},"output":[],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}}`,
				`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
			} {
				pool := responsebuffer.NewPool(2 << 20)
				budget := pool.Request(1 << 20)
				var source io.ReadCloser = io.NopCloser(strings.NewReader("data: " + payload + "\n\n"))
				if canonical {
					source = responseflow.New(source, budget)
				}
				replay, verdict, usage, fp, err := peekQualityStreamReport(responsebuffer.WithContext(t.Context(), budget), source, protocol, QualityRetryRuntime{})
				replay.Close()
				var failure *qualityUpstreamFailure
				if verdict != QualityWait || !errors.As(err, &failure) || errors.Is(err, errQualityEmptyStream) || !fp.Failed || fp.Completed || fp.Rule != "upstream_error" {
					t.Fatalf("canonical=%v protocol=%s verdict=%s fp=%+v error=%v", canonical, protocol, verdict, fp, err)
				}
				if strings.Contains(string(failure.diagnostic.Body), "must-not-be-audited") || strings.Contains(err.Error(), "synthetic") {
					t.Fatal("error exposed output or message outside audit")
				}
				if protocol == qualityProtocolResponses && strings.Contains(payload, `"input_tokens"`) && (!usage.Reported || usage.InputTokens != 12 || usage.OutputTokens != 3) {
					t.Fatalf("lost usage on failure: %+v", usage)
				}
				if pool.Snapshot().Used != 0 {
					t.Fatal("failed stream leaked budget")
				}
			}
		}
	}
}

func TestExplicitInvalidArgumentDoesNotRotateOrCoolAccount(t *testing.T) {
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, credentials, audits, _ := newGuardLoopServiceWithDB(t, adapter, "synthetic-first", "synthetic-second")
	adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: "data: {\"type\":\"error\",\"error\":{\"code\":\"invalid-argument\",\"message\":\"synthetic unsupported effort\"},\"output\":\"must-not-be-audited\"}\n\n"}}
	result, err := service.CreateChatCompletion(t.Context(), guardLoopInput("synthetic-error", true))
	if result != nil {
		result.Body.Close()
		t.Fatal("failed event delivered")
	}
	var failure *UpstreamFailure
	if !errors.As(err, &failure) || failure.HTTPStatus != 400 || failure.Code != "upstream_bad_request" {
		t.Fatalf("wrong result: %v", err)
	}
	if len(adapter.Attempts()) != 1 {
		t.Fatalf("invalid argument rotated accounts: %v", adapter.Attempts())
	}
	view, err := service.accounts.Get(t.Context(), credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Credential.CooldownUntil != nil || view.Credential.LastError != "" || !service.selector.LocalQualityAllowed(credentials[0].ID, time.Now()) {
		t.Fatal("explicit failure punished account as empty/degraded")
	}
	records, _, err := audits.List(t.Context(), 0, 20)
	if err != nil || len(records) != 1 {
		t.Fatalf("audit: %v (%d)", err, len(records))
	}
	detail, err := audits.Get(t.Context(), records[0].ID)
	if err != nil || len(detail.Attempts) != 1 {
		t.Fatalf("attempt diagnostic missing: %v", err)
	}
	diagnostic := detail.Attempts[0]
	if diagnostic.Stage != "response_stream" || !strings.Contains(string(diagnostic.ResponseBody), "invalid-argument") || strings.Contains(string(diagnostic.ResponseBody), "must-not-be-audited") {
		t.Fatalf("invalid diagnostic: %+v", diagnostic)
	}
}

func TestQualityFailedJSONDoesNotAdmitThinking(t *testing.T) {
	for _, payload := range []string{
		`{"status":"failed","error":{"code":"server_error","message":"synthetic failure"},"output":[{"type":"reasoning","summary":[{"text":"plan"}]}],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}`,
		`{"type":"error","error":{"type":"invalid_request_error","message":"synthetic failure"}}`,
	} {
		body, verdict, _, _, err := peekQualityBodyReportWithBudget(io.NopCloser(strings.NewReader(payload)), QualityRetryRuntime{}, responsebuffer.NewPool(2<<20).Request(1<<20))
		body.Close()
		if verdict != QualityWait || !errors.Is(err, errQualityUpstreamFailure) {
			t.Fatalf("failed response admitted: %s, %v", verdict, err)
		}
	}
}
