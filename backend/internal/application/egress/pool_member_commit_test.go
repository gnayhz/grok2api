package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

type poolMemberCommitGate struct {
	*relational.EgressRepository
	before func(context.Context) error
}

func poolMemberDeadProbe(sequence uint64) domain.ProbeResult {
	now := time.Now().UTC()
	family := domain.ProbeFamilyResult{Status: domain.ProbeStatusUnhealthy, TestedAt: now, Error: "synthetic transport failure"}
	return domain.ProbeResult{Revision: sequence, Status: domain.ProbeStatusUnhealthy, TestedAt: now, IPv4: family, IPv6: family}
}

func (r *poolMemberCommitGate) SetEgressPoolMembers(ctx context.Context, poolID uint64, ids []uint64) error {
	if err := r.before(ctx); err != nil {
		return err
	}
	return r.EgressRepository.SetEgressPoolMembers(ctx, poolID, ids)
}

func TestPoolMemberCommitRejectsDeletedParent(t *testing.T) {
	for _, deleted := range []string{"pool", "node", "nodes_batch", "unhealthy_nodes"} {
		t.Run(deleted, func(t *testing.T) {
			ctx, original, repo := newFinalReviewHealthFixture(t)
			defer original.Close(ctx)
			bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			pool, err := original.CreatePool(ctx, PoolInput{Name: t.Name() + "-pool"})
			if err != nil {
				t.Fatal(err)
			}
			defer original.DeletePool(ctx, pool.ID)
			proxy := "http://member.example:8080"
			node, err := original.Create(ctx, Input{Name: t.Name() + "-node", Enabled: true, ProxyURL: &proxy})
			if err != nil {
				t.Fatal(err)
			}
			defer original.Delete(ctx, node.ID)
			ready, release, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			gate := &poolMemberCommitGate{EgressRepository: repo, before: func(ctx context.Context) error {
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
			go func() { result <- service.SetPoolMembers(bounded, pool.ID, []uint64{node.ID}) }()
			select {
			case <-ready:
			case <-bounded.Done():
				t.Fatal(bounded.Err())
			}
			switch deleted {
			case "pool":
				err = original.DeletePool(ctx, pool.ID)
			case "node":
				err = original.Delete(ctx, node.ID)
			case "nodes_batch":
				_, err = original.DeleteMany(ctx, []uint64{node.ID})
			default:
				// The cleanup predicate is persisted transport health; use the
				// regular probe writer before asking the real bulk cleanup path.
				binding, readErr := repo.GetEgressNode(ctx, node.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				sequence, beginErr := repo.BeginEgressNodeProbe(ctx, node.ID, binding.EncryptedProxyURL)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				probe := poolMemberDeadProbe(sequence)
				if err := repo.UpdateEgressNodeProbe(ctx, node.ID, binding.EncryptedProxyURL, probe); err != nil {
					t.Fatal(err)
				}
				_, err = original.DeleteUnhealthy(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-result; !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalidInput) {
				t.Errorf("deleted %s membership save = %v", deleted, err)
			}
			members, err := repo.EgressPoolMembers(ctx)
			if err != nil || len(members[pool.ID]) != 0 {
				t.Errorf("deleted %s left ghost members: %+v %v", deleted, members, err)
			}
		})
	}
}
