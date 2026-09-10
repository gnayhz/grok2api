package gateway

import (
	"context"
	"errors"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestServerToolRequestsNeverRepeatAfterAmbiguousFailure(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError, http.StatusUnauthorized} {
		adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
		s, accounts := newGuardLoopService(t, adapter, "tool-first", "tool-second")
		adapter.responses[accounts[0].ID] = []scriptedBuildResponse{{status: status, body: "data: {\"choices\":[{\"delta\":{\"content\":\"bare\"}}]}\n\n"}}
		adapter.checkRequest = func(request provider.ResponseResourceRequest) {
			if !request.DisableAutomaticReplay {
				t.Error("unsafe request enabled adapter fallback")
			}
		}
		input := guardLoopInput("side-effect", true)
		input.Body = []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"execute"}],"tools":[{"type":"mcp"}]}`)
		result, err := s.CreateChatCompletion(context.Background(), input)
		if result != nil {
			result.Body.Close()
		}
		if err == nil || len(adapter.Attempts()) != 1 {
			t.Fatalf("status=%d err=%v attempts=%v", status, err, adapter.Attempts())
		}
	}
}

func TestAdmissionUsesNormalizedToolProfileBeforeNetwork(t *testing.T) {
	for _, actualTools := range []bool{false, true} {
		base := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
		s, accounts := newGuardLoopService(t, base, "normalized-budget")
		s.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, AdmissionTimeout: 50 * time.Millisecond, ToolAdmissionTimeout: 250 * time.Millisecond})
		base.responses[accounts[0].ID] = []scriptedBuildResponse{{status: 200, headerDelay: 110 * time.Millisecond, body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan\"}}]}\n\ndata: [DONE]\n\n"}}
		s.providers = provider.NewRegistry(resourceTestAdapter{base, func(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
			policy := inferencedomain.ReplayPolicy{Safe: true, Tools: actualTools}
			request.NormalizedMetadata.ReplayPolicy = &policy
			if err := request.OnNormalized(*request.NormalizedMetadata); err != nil {
				return nil, err
			}
			return base.ForwardResponse(ctx, request)
		}})
		input := guardLoopInput("actual-tools", true)
		if !actualTools {
			input.Body = []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","name":"f"}]}`)
		}
		result, err := s.CreateChatCompletion(context.Background(), input)
		if actualTools {
			if err != nil {
				t.Fatalf("normalized tools lost larger budget: %v", err)
			}
			_, _ = io.Copy(io.Discard, result.Body)
			finishTestResult(t, result, Usage{}, "", "")
			result.Body.Close()
		} else {
			var failure *UpstreamFailure
			if result != nil || !errors.As(err, &failure) || failure.Code != "quality_admission_timeout" {
				t.Fatalf("normalized no-tools retained raw tool budget: result=%v err=%v", result, err)
			}
		}
	}
}

func TestNormalizedProviderPolicyCanForbidReplay(t *testing.T) {
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	s, accounts := newGuardLoopService(t, adapter, "normalized-tool", "unused-tool")
	adapter.responses[accounts[0].ID] = []scriptedBuildResponse{{status: 200, body: "data: {\"choices\":[{\"delta\":{\"content\":\"bare\"}}]}\n\n"}}
	adapter.checkRequest = func(request provider.ResponseResourceRequest) {
		request.NormalizedMetadata.ReplayPolicy = &inferencedomain.ReplayPolicy{Tools: true, Reason: "normalized_server_tool"}
	}
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("normalized-tools", true))
	if result != nil {
		result.Body.Close()
	}
	if err == nil || len(adapter.Attempts()) != 1 {
		t.Fatalf("err=%v attempts=%v", err, adapter.Attempts())
	}
}
