package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestQualityProbeUsesProductionAccountCapacity(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1}}}
	limiter := memory.NewConcurrencyLimiter()
	selector := NewSelector(repo, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	release, ok, err := limiter.Acquire(context.Background(), accountConcurrencyKey(1), 1)
	if err != nil || !ok {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	lease, err := selector.AcquirePinnedForQualityProbe(ctx, account.ProviderBuild, 1, 0, "model-a", "", clientkey.AccountScope{})
	if lease != nil {
		lease.Release()
		t.Fatal("probe exceeded active production capacity")
	}
	if err == nil {
		t.Fatal("probe capacity failure invisible")
	}
	release()
	lease, err = selector.AcquirePinnedForQualityProbe(context.Background(), account.ProviderBuild, 1, 0, "model-a", "", clientkey.AccountScope{})
	if err != nil || lease == nil {
		t.Fatalf("released production slot unavailable: %v", err)
	}
	defer lease.Release()
	if release, ok, err := limiter.Acquire(context.Background(), accountConcurrencyKey(1), 1); err != nil || ok {
		if ok {
			release()
		}
		t.Fatalf("production did not see active probe: acquired=%v err=%v", ok, err)
	}
}

func TestProbeBudgetAndIdentityAreSharedAcrossSelectors(t *testing.T) {
	limiter := memory.NewConcurrencyLimiter()
	first, second := &Selector{concurrency: limiter}, &Selector{concurrency: limiter}
	ctx := context.Background()
	release, err := first.acquireProbeResources(qualitymodel.WithProbeIdentity(ctx, 9, true), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if foreignRelease, err := second.acquireProbeResources(qualitymodel.WithProbeIdentity(deadline, 9, true), 2); err == nil {
		foreignRelease()
		t.Fatal("aliases overlapped across instances")
	}
	if count, _ := limiter.Current(ctx, "quality:identity/account/2"); count != 0 {
		t.Fatal("failed group acquisition leaked stable account slot")
	}
	otherRelease, err := second.acquireProbeResources(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer otherRelease()
	deadline2, cancel2 := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel2()
	if extraRelease, err := second.acquireProbeResources(deadline2, 4); !errors.Is(err, context.DeadlineExceeded) {
		if extraRelease != nil {
			extraRelease()
		}
		t.Fatalf("probe limit ignored: %v", err)
	}
	if count, _ := limiter.Current(ctx, "quality:identity/account/4"); count != 0 {
		t.Fatal("failed probe budget leaked identity slot")
	}
}

func TestFrozenProbeSelectsOriginalModelAndNormalizedEffort(t *testing.T) {
	baseline := attemptmeta.Identity{ID: "trigger/1", Provider: "grok_build", Model: "grok-4.6", RuleVersion: "rules", Revision: 3,
		Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "high"}}
	spec := qualitymodel.NewProbeExperiment(qualitymodel.Observation{EventID: "trigger/1/admission", Attempt: baseline})
	ctx := qualitymodel.WithProbeExperiment(context.Background(), spec)
	resolver := &pagedQualityProbeRouteResolver{routes: []modeldomain.Route{
		{ID: 1, Provider: account.ProviderBuild, PublicID: "grok-4.5", UpstreamModel: "grok-4.5"},
		{ID: 2, Provider: account.ProviderBuild, PublicID: "grok-4.6", UpstreamModel: "grok-4.6"},
	}}
	svc := &Service{models: resolver}
	route, reason := svc.qualityProbeRoute(ctx)
	if reason != nil || route.ID != 2 {
		t.Fatalf("wrong model selected: %+v %q", route, reason)
	}
	request, err := svc.qualityProbeRequestForContext(ctx, route, account.Credential{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input     string `json:"input"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Input != spec.Prompt() || body.Reasoning.Effort != "high" {
		t.Fatalf("experiment changed: %+v", body)
	}
	resolver.routes = resolver.routes[:1]
	if route, _ := svc.qualityProbeRoute(ctx); route.Provider != "" {
		t.Fatal("missing model silently replaced")
	}
}
