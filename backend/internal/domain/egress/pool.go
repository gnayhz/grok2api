package egress

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidPoolFallback = errors.New("代理池回退目标无效")
	ErrInvalidPoolMember   = errors.New("代理池成员无效")
)

// ValidatePoolFallback checks the proposed edge against the current pool graph.
// The writer must keep this snapshot stable until the mutation commits. A new
// pool has ID zero; every pool reached by its fallback must already exist.
func ValidatePoolFallback(value Pool, pools []Pool) error {
	if value.FallbackMode.Normalized() != PoolFallbackPool {
		return nil
	}
	byID := make(map[uint64]Pool, len(pools))
	for _, pool := range pools {
		byID[pool.ID] = pool
	}
	visited := make(map[uint64]bool)
	if value.ID != 0 {
		visited[value.ID] = true
	}
	next := value.FallbackPoolID
	for {
		if next == 0 {
			return fmt.Errorf("%w: 必须指定回退代理池", ErrInvalidPoolFallback)
		}
		if visited[next] {
			return fmt.Errorf("%w: 回退链不能成环", ErrInvalidPoolFallback)
		}
		visited[next] = true
		pool, exists := byID[next]
		if !exists {
			return fmt.Errorf("%w: 回退代理池不存在", ErrInvalidPoolFallback)
		}
		if pool.FallbackMode.Normalized() != PoolFallbackPool {
			return nil
		}
		next = pool.FallbackPoolID
	}
}
