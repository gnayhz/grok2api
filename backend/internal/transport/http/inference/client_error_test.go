package inference

import (
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"net/http"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
)

func TestClassifyClientErrorMapsPublicContract(t *testing.T) {
	view := classifyClientError(gateway.ErrLedgerUnavailable)
	if view.Status != http.StatusServiceUnavailable || view.Code != "ledger_unavailable" {
		t.Fatalf("ledger: %+v", view)
	}
	view = classifyClientError(clientkeyapp.ErrModelNotAllowed)
	if view.Status != http.StatusForbidden || view.Code != "model_not_allowed" || view.AnthropicType != "permission_error" {
		t.Fatalf("model: %+v", view)
	}
	view = classifyClientError(&gateway.UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: gateway.ErrorQualityDegraded, PublicMessage: "degraded"})
	if view.Code != "upstream_degraded" || view.Status != http.StatusServiceUnavailable {
		t.Fatalf("quality: %+v", view)
	}
	view = classifyClientError(&gateway.UpstreamFailure{HTTPStatus: http.StatusPaymentRequired, QuotaExhausted: true, UpstreamCode: "billing"})
	if view.Status != http.StatusServiceUnavailable || view.Code != "upstream_unavailable" {
		t.Fatalf("sanitized billing: %+v", view)
	}
	view = classifyClientError(&selector.SelectionUnavailableError{Reason: selector.SelectionCooling, RetryAfter: 1500 * time.Millisecond})
	if view.Message != "上游账号正在冷却" || view.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("selection: %+v", view)
	}
}
