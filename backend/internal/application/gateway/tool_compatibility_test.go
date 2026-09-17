package gateway

import (
	"context"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"testing"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestGatewayReviewsCacheToolPlanBeforeUpstream(t *testing.T) {
	base := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	s, _ := newGuardLoopService(t, base, "tool-plan-first", "tool-plan-second")
	reviews := 0
	s.providers = providerimpl.NewRegistry(resourceTestAdapter{base, func(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
		reviews++
		if request.ToolCompatibilityPolicy != inferencedomain.AllowDisabledCacheTools {
			t.Error("logical request owner did not supply cache policy")
		}
		err := request.OnNormalized(provider.NormalizedRequestMetadata{ToolCompatibility: &inferencedomain.ToolCompatibilityPlan{AddedCacheTools: []string{"x_search"}, ExecutionDisabled: false}})
		if err == nil {
			t.Fatal("gateway accepted a callable cache-only tool")
		}
		return nil, err
	}})
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("tool-plan", true))
	if result != nil {
		result.Body.Close()
	}
	if err == nil || reviews == 0 || len(base.Attempts()) != 0 {
		t.Fatalf("err=%v reviews=%d upstream=%v", err, reviews, base.Attempts())
	}
}
