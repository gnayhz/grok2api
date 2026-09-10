package egress

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

type poolGraphGateRepository struct {
	*relational.EgressRepository
	beforeUpdate func(context.Context) error
}

func (r *poolGraphGateRepository) UpdateEgressPool(ctx context.Context, value domain.Pool) (domain.Pool, error) {
	if err := r.beforeUpdate(ctx); err != nil {
		return domain.Pool{}, err
	}
	return r.EgressRepository.UpdateEgressPool(ctx, value)
}

func TestPoolGraphRejectsMissingFallback(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			ctx, service, repo := newFinalReviewHealthFixture(t)
			defer service.Close(ctx)
			name := t.Name()
			input := PoolInput{Name: name, Strategy: domain.PoolStrategyRandom, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 999999999}
			if operation == "create" {
				created, err := service.CreatePool(ctx, input)
				if !errors.Is(err, ErrInvalidInput) {
					t.Errorf("missing fallback was accepted: pool=%+v err=%v", created, err)
				}
				if created.ID != 0 {
					_ = service.DeletePool(ctx, created.ID)
				}
				return
			}
			created, err := service.CreatePool(ctx, PoolInput{Name: name, Strategy: domain.PoolStrategyRandom})
			if err != nil {
				t.Fatal(err)
			}
			defer service.DeletePool(ctx, created.ID)
			if _, err := service.UpdatePool(ctx, created.ID, input); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("missing fallback update = %v", err)
			}
			stored, err := repo.GetEgressPool(ctx, created.ID)
			if err != nil || stored.FallbackMode != domain.PoolFallbackNone || stored.FallbackPoolID != 0 {
				t.Errorf("rejected update changed pool: %+v %v", stored, err)
			}
		})
	}
}

func TestPoolGraphConcurrentEdgesCannotCommitCycle(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			baseCtx, original, repo := newFinalReviewHealthFixture(t)
			defer original.Close(baseCtx)
			ctx, cancel := context.WithTimeout(baseCtx, 15*time.Second)
			defer cancel()
			pools := make([]domain.PublicPool, count)
			for i := range pools {
				created, err := original.CreatePool(ctx, PoolInput{Name: fmt.Sprintf("%s-%d", t.Name(), i), Strategy: domain.PoolStrategyRandom})
				if err != nil {
					t.Fatal(err)
				}
				pools[i] = created
				defer original.DeletePool(baseCtx, created.ID)
			}
			ready, release, results := make(chan struct{}, count), make(chan struct{}), make(chan error, count)
			for i := range pools {
				gate := &poolGraphGateRepository{EgressRepository: repo, beforeUpdate: func(ctx context.Context) error {
					ready <- struct{}{}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}}
				service := NewService(gate, original.cipher)
				defer service.Close(baseCtx)
				go func() {
					_, err := service.UpdatePool(ctx, pools[i].ID, PoolInput{Name: pools[i].Name, Strategy: domain.PoolStrategyRandom, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: pools[(i+1)%count].ID})
					results <- err
				}()
			}
			for range count {
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("updates did not reach repository commit")
				}
			}
			close(release)
			accepted, rejected := 0, 0
			for range count {
				err := <-results
				switch {
				case err == nil:
					accepted++
				case errors.Is(err, ErrInvalidInput):
					rejected++
				default:
					t.Errorf("unexpected save failure: %v", err)
				}
			}
			if accepted != count-1 || rejected != 1 {
				t.Errorf("concurrent edges accepted=%d rejected=%d, want %d/1", accepted, rejected, count-1)
			}
			current := pools[0].ID
			visited := make(map[uint64]bool)
			for current != 0 {
				if visited[current] {
					t.Errorf("committed fallback cycle at pool %d", current)
					break
				}
				visited[current] = true
				pool, err := repo.GetEgressPool(ctx, current)
				if err != nil {
					t.Fatal(err)
				}
				current = pool.FallbackPoolID
			}
		})
	}
}

func TestPoolGraphDeletedFallbackCannotBeRepublished(t *testing.T) {
	baseCtx, original, repo := newFinalReviewHealthFixture(t)
	defer original.Close(baseCtx)
	ctx, cancel := context.WithTimeout(baseCtx, 15*time.Second)
	defer cancel()
	first, err := original.CreatePool(ctx, PoolInput{Name: t.Name() + "-first"})
	if err != nil {
		t.Fatal(err)
	}
	defer original.DeletePool(baseCtx, first.ID)
	second, err := original.CreatePool(ctx, PoolInput{Name: t.Name() + "-second"})
	if err != nil {
		t.Fatal(err)
	}
	ready, release, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	gate := &poolGraphGateRepository{EgressRepository: repo, beforeUpdate: func(ctx context.Context) error {
		close(ready)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	service := NewService(gate, original.cipher)
	defer service.Close(baseCtx)
	go func() {
		_, err := service.UpdatePool(ctx, first.ID, PoolInput{Name: first.Name, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: second.ID})
		result <- err
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("update did not reach repository commit")
	}
	if err := original.DeletePool(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrInvalidInput) {
		t.Errorf("deleted fallback save = %v", err)
	}
	stored, err := repo.GetEgressPool(ctx, first.ID)
	if err != nil || stored.FallbackPoolID != 0 || stored.FallbackMode != domain.PoolFallbackNone {
		t.Errorf("deleted fallback was republished: %+v %v", stored, err)
	}
}
