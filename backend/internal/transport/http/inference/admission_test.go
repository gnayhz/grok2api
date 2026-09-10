package inference

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/gin-gonic/gin"
)

func TestAdmissionFailurePrecedesResponseHeadersAndBody(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		finalCode := ""
		result := &gateway.Result{StatusCode: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Upstream-Only": []string{"private"}},
			Body:   io.NopCloser(strings.NewReader("UPSTREAM_BYTES_MUST_NOT_LEAK")),
			CommitDelivery: func() error {
				return &gateway.UpstreamFailure{HTTPStatus: http.StatusGatewayTimeout, Code: "quality_admission_timeout", PublicMessage: "admission expired"}
			},
			Finalize: func(_ gateway.Usage, _, code string) { finalCode = code },
		}
		(&Handler{}).writeProtocolResult(ctx, result, true, anthropic, streamProtocolResponses, "")
		if recorder.Code != http.StatusGatewayTimeout || strings.Contains(recorder.Body.String(), "UPSTREAM_BYTES") || recorder.Header().Get("X-Upstream-Only") != "" || finalCode != "quality_admission_timeout" {
			t.Fatalf("anthropic=%v status=%d headers=%v body=%s final=%s", anthropic, recorder.Code, recorder.Header(), recorder.Body.String(), finalCode)
		}
	}
}

func TestTransportPreflightFailureDoesNotCommitDelivery(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	committed := false
	result := &gateway.Result{StatusCode: 200, Header: http.Header{"Content-Length": []string{"999999999999"}}, Body: io.NopCloser(strings.NewReader("large")),
		CommitDelivery: func() error { committed = true; return nil }, Finalize: func(gateway.Usage, string, string) {}}
	(&Handler{}).writeProtocolResult(ctx, result, false, false, streamProtocolResponses, "")
	if committed || recorder.Code != http.StatusBadGateway {
		t.Fatalf("committed=%v status=%d", committed, recorder.Code)
	}
}
