package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

type nodeRoutingCommitGate struct {
	*relational.EgressRepository
	before func(context.Context) error
}

func (r *nodeRoutingCommitGate) UpdateEgressNodeConfiguration(ctx context.Context, node domain.Node, validate domain.FixedTargetValidator) (domain.Node, error) {
	if err := r.before(ctx); err != nil {
		return domain.Node{}, err
	}
	return r.EgressRepository.UpdateEgressNodeConfiguration(ctx, node, validate)
}

func TestNodeRoutingCommitRejectsNewlyReferencedInvalidNode(t *testing.T) {
	for _, level := range []string{"default", "scope", "class"} {
		for _, change := range []string{"disable", "clear_proxy", "account_template"} {
			t.Run(level+"/"+change, func(t *testing.T) {
				ctx, original, repo := newFinalReviewHealthFixture(t)
				defer original.Close(ctx)
				bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				proxy := "http://fixed-target.example:8080"
				node, err := original.Create(ctx, Input{Name: t.Name(), Enabled: true, ProxyURL: &proxy})
				if err != nil {
					t.Fatal(err)
				}
				defer original.Delete(ctx, node.ID)
				ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				gate := &nodeRoutingCommitGate{EgressRepository: repo, before: func(ctx context.Context) error {
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
				input := Input{Name: node.Name, Enabled: true}
				switch change {
				case "disable":
					input.Enabled = false
				case "clear_proxy":
					input.ClearProxyURL = true
				default:
					template := "http://user-{account}:password@fixed-target.example:8080"
					input.ProxyURL = &template
				}
				go func() { _, err := service.Update(bounded, node.ID, input); done <- err }()
				select {
				case <-ready:
				case err := <-done:
					t.Fatalf("update failed before commit barrier: %v", err)
				case <-bounded.Done():
					t.Fatal(bounded.Err())
				}
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
				if _, err := original.UpdateOperationsConfig(ctx, config); err != nil {
					t.Fatal(err)
				}
				close(release)
				if err := <-done; !errors.Is(err, ErrInvalidInput) {
					t.Errorf("newly referenced invalid node update=%v", err)
				}
				stored, err := repo.GetEgressNode(ctx, node.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := original.validateFixedTargetNode(stored); err != nil {
					t.Errorf("committed unusable fixed target: %v", err)
				}
			})
		}
	}
}

func TestMissingPoolDeletionKeepsApplicationNotFound(t *testing.T) {
	ctx, service, _ := newFinalReviewHealthFixture(t)
	defer service.Close(ctx)
	pool, err := service.CreatePool(ctx, PoolInput{Name: "missing-deletion"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeletePool(ctx, pool.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.DeletePool(ctx, pool.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second deletion error=%v, want application NotFound", err)
	}
}

func TestNodeUpdateDeletedAtCommitKeepsApplicationNotFound(t *testing.T) {
	ctx, original, repo := newFinalReviewHealthFixture(t)
	defer original.Close(ctx)
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	proxy := "http://deleted-target.example:8080"
	node, err := original.Create(ctx, Input{Name: "delete-at-commit", Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	gate := &nodeRoutingCommitGate{EgressRepository: repo, before: func(ctx context.Context) error {
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
	go func() {
		_, err := service.Update(bounded, node.ID, Input{Name: "renamed-before-delete", Enabled: true})
		done <- err
	}()
	select {
	case <-ready:
	case <-bounded.Done():
		t.Fatal(bounded.Err())
	}
	if err := original.Delete(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted update error=%v, want application NotFound", err)
	}
}
