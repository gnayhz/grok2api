package egress

import (
	"context"
	"errors"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"testing"
	"time"
)

type routingTargetCommitGate struct {
	*relational.EgressRepository
	before func(context.Context) error
}

func (r *routingTargetCommitGate) SaveEgressOperationsConfig(ctx context.Context, value domain.OperationsConfig, validate domain.FixedTargetValidator) (domain.OperationsConfig, error) {
	if err := r.before(ctx); err != nil {
		return domain.OperationsConfig{}, err
	}
	return r.EgressRepository.SaveEgressOperationsConfig(ctx, value, validate)
}
func TestRoutingTargetCommitRejectsCurrentInvalidNode(t *testing.T) {
	for _, level := range []string{"default", "scope", "class"} {
		for _, change := range []string{"disable", "clear_proxy", "account_template"} {
			t.Run(level+"/"+change, func(t *testing.T) {
				ctx, original, repo := newFinalReviewHealthFixture(t)
				defer original.Close(ctx)
				bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				// Explicit direct configuration prevents another fixture's old target
				// from deciding this test's update eligibility.
				direct := RoutingTargetInput{Mode: domain.RoutingTargetDirect}
				if _, err := original.UpdateOperationsConfig(ctx, OperationsConfigInput{ProbeIntervalSeconds: 900, DefaultTarget: &direct, ScopeTargets: map[domain.Scope]RoutingTargetInput{}, ClassTargets: map[domain.TrafficClass]RoutingTargetInput{}}); err != nil {
					t.Fatal(err)
				}
				proxy := "http://current-target.example:8080"
				node, err := original.Create(ctx, Input{Name: t.Name(), Enabled: true, ProxyURL: &proxy})
				if err != nil {
					t.Fatal(err)
				}
				defer original.Delete(ctx, node.ID)
				ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				gate := &routingTargetCommitGate{EgressRepository: repo, before: func(ctx context.Context) error {
					close(ready)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}}
				service := NewService(gate, original.cipher)
				defer service.Close(ctx)
				config := OperationsConfigInput{ProbeIntervalSeconds: 900}
				target := RoutingTargetInput{Mode: domain.RoutingTargetNode, NodeID: node.ID}
				switch level {
				case "default":
					config.DefaultTarget = &target
				case "scope":
					config.ScopeTargets = map[domain.Scope]RoutingTargetInput{domain.ScopeWeb: target}
				default:
					config.ClassTargets = map[domain.TrafficClass]RoutingTargetInput{domain.TrafficClassInference: target}
				}
				go func() { _, err := service.UpdateOperationsConfig(bounded, config); done <- err }()
				select {
				case <-ready:
				case err := <-done:
					t.Fatalf("save failed before commit: %v", err)
				case <-bounded.Done():
					t.Fatal(bounded.Err())
				}
				input := Input{Name: node.Name, Enabled: true}
				switch change {
				case "disable":
					input.Enabled = false
				case "clear_proxy":
					input.ClearProxyURL = true
				default:
					template := "http://user-{account}:password@current-target.example:8080"
					input.ProxyURL = &template
				}
				if _, err := original.Update(ctx, node.ID, input); err != nil {
					t.Fatal(err)
				}
				close(release)
				if err := <-done; !errors.Is(err, ErrInvalidInput) {
					t.Errorf("current invalid target accepted: %v", err)
				}
				stored, err := repo.GetEgressOperationsConfig(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if stored.DefaultTarget.Mode != domain.RoutingTargetDirect || len(stored.ScopeTargets) != 0 || len(stored.ClassTargets) != 0 {
					t.Errorf("invalid target replaced previous routing: %+v", stored)
				}
			})
		}
	}
}
