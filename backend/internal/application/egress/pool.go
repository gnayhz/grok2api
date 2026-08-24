package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// poolStore is the persistence surface dedicated pools need.
type poolStore interface {
	ListEgressPools(ctx context.Context) ([]domain.Pool, error)
	GetEgressPool(ctx context.Context, id uint64) (domain.Pool, error)
	CreateEgressPool(ctx context.Context, value domain.Pool) (domain.Pool, error)
	UpdateEgressPool(ctx context.Context, value domain.Pool) (domain.Pool, error)
	DeleteEgressPool(ctx context.Context, id uint64) error
	EgressPoolMembers(ctx context.Context) (map[uint64][]uint64, error)
	SetEgressPoolMembers(ctx context.Context, poolID uint64, nodeIDs []uint64) error
	SetEgressPoolMemberPriority(ctx context.Context, poolID, nodeID uint64, priority int64) error
}

// PoolInput is the create/update payload for a dedicated pool.
type PoolInput struct {
	Name           string
	Enabled        *bool
	Strategy       domain.PoolStrategy
	FallbackMode   domain.PoolFallbackMode
	FallbackPoolID uint64
}

// ListPools returns public pools with member summaries.
func (s *Service) ListPools(ctx context.Context) ([]domain.PublicPool, error) {
	store, err := s.poolStore()
	if err != nil {
		return nil, err
	}
	pools, err := store.ListEgressPools(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := s.repository.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		return nil, err
	}
	members, err := store.EgressPoolMembers(ctx)
	if err != nil {
		return nil, err
	}
	var preferred map[uint64]uint64
	if preferredStore, ok := store.(interface {
		EgressPoolPreferredNodes(ctx context.Context) (map[uint64]uint64, error)
	}); ok {
		preferred, _ = preferredStore.EgressPoolPreferredNodes(ctx)
	}
	now := time.Now().UTC()
	byName := make(map[uint64]string, len(pools))
	for _, pool := range pools {
		byName[pool.ID] = pool.Name
	}
	byNode := make(map[uint64]domain.Node, len(nodes))
	for _, node := range nodes {
		byNode[node.ID] = node
	}
	result := make([]domain.PublicPool, 0, len(pools))
	for _, pool := range pools {
		public := domain.PublicPool{
			ID: pool.ID, Name: pool.Name, Enabled: pool.Enabled,
			Strategy:     pool.Strategy.Normalized(),
			FallbackMode: pool.FallbackMode.Normalized(), FallbackPoolID: pool.FallbackPoolID,
			FallbackPoolName: byName[pool.FallbackPoolID],
			CreatedAt:        pool.CreatedAt, UpdatedAt: pool.UpdatedAt,
			MemberIDs:            members[pool.ID],
			PreferredNodeID:      preferred[pool.ID],
			RotationCursorNodeID: pool.RotationCursorNodeID,
		}
		for _, nodeID := range members[pool.ID] {
			node, found := byNode[nodeID]
			if !found {
				continue
			}
			public.MemberCount++
			if node.ProbeStatus == domain.ProbeStatusHealthy && (node.CooldownUntil == nil || !now.Before(*node.CooldownUntil)) {
				public.HealthyCount++
			}
			// 口径:QuarantinedCount 只统计出口质量隔离(LastError==exit_ip_quality)。
			// 此前把传输层普通冷却也计入, UI 上与"已隔离"混标会让运维高估质量
			// 守卫的命中规模; 冷却中的节点已由 HealthyCount 的条件排除体现。
			if node.LastError == domain.LastErrorExitIPQuality {
				public.QuarantinedCount++
			}
		}
		result = append(result, public)
	}
	return result, nil
}

// CreatePool validates and creates a dedicated pool.
func (s *Service) CreatePool(ctx context.Context, input PoolInput) (domain.PublicPool, error) {
	store, err := s.poolStore()
	if err != nil {
		return domain.PublicPool{}, err
	}
	if err := s.validatePoolInput(ctx, store, input, nil); err != nil {
		return domain.PublicPool{}, err
	}
	pool, err := store.CreateEgressPool(ctx, domain.Pool{
		Name: strings.TrimSpace(input.Name), Enabled: input.Enabled == nil || *input.Enabled,
		Strategy: input.Strategy, FallbackMode: input.FallbackMode, FallbackPoolID: normalizedPoolFallback(input.FallbackMode, input.FallbackPoolID),
	})
	if err != nil {
		return domain.PublicPool{}, err
	}
	return s.publicPool(ctx, pool)
}

// UpdatePool validates and updates one pool. Fallback cycle detection walks
// the whole chain so A→B→A configurations are rejected before persisting.
func (s *Service) UpdatePool(ctx context.Context, id uint64, input PoolInput) (domain.PublicPool, error) {
	store, err := s.poolStore()
	if err != nil {
		return domain.PublicPool{}, err
	}
	current, err := store.GetEgressPool(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return domain.PublicPool{}, ErrNotFound
		}
		return domain.PublicPool{}, err
	}
	if err := s.validatePoolInput(ctx, store, input, &current); err != nil {
		return domain.PublicPool{}, err
	}
	updated, err := store.UpdateEgressPool(ctx, domain.Pool{
		ID: id, Name: strings.TrimSpace(input.Name), Enabled: input.Enabled == nil || *input.Enabled,
		Strategy: input.Strategy, FallbackMode: input.FallbackMode, FallbackPoolID: normalizedPoolFallback(input.FallbackMode, input.FallbackPoolID),
	})
	if err != nil {
		return domain.PublicPool{}, err
	}
	s.invalidatePoolCache()
	return s.publicPool(ctx, updated)
}

// DeletePool removes a pool; members are detached (they return to the
// automatic schedule) and routing references are cleaned up atomically.
func (s *Service) DeletePool(ctx context.Context, id uint64) error {
	store, err := s.poolStore()
	if err != nil {
		return err
	}
	if err := store.DeleteEgressPool(ctx, id); err != nil {
		return err
	}
	s.invalidateOperationsConfig()
	s.invalidatePoolCache()
	return nil
}

// SetPoolMembers replaces the full membership of one pool. Membership is
// edited pool-side only: a node may belong to several pools at once.
func (s *Service) SetPoolMembers(ctx context.Context, poolID uint64, nodeIDs []uint64) error {
	store, err := s.poolStore()
	if err != nil {
		return err
	}
	if _, err := store.GetEgressPool(ctx, poolID); err != nil {
		// repository.ErrNotFound 直接透传会被 writeError 落到 500 分支;
		// 与其他服务方法一致地归一为应用层 ErrNotFound(404)。
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := s.validateMemberNodes(ctx, nodeIDs); err != nil {
		return err
	}
	if err := store.SetEgressPoolMembers(ctx, poolID, nodeIDs); err != nil {
		return err
	}
	s.invalidateOperationsConfig()
	s.invalidatePoolCache()
	return nil
}

// validateMemberNodes rejects unknown node ids up front: the member table has
// no foreign key, and a ghost row would echo back in MemberIDs while every
// other view silently skips it.
func (s *Service) validateMemberNodes(ctx context.Context, nodeIDs []uint64) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	nodes, err := s.repository.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		return err
	}
	known := make(map[uint64]struct{}, len(nodes))
	for _, node := range nodes {
		known[node.ID] = struct{}{}
	}
	for _, id := range nodeIDs {
		if _, ok := known[id]; !ok {
			return fmt.Errorf("%w: 节点 %d 不存在", ErrInvalidInput, id)
		}
	}
	return nil
}

// SetPoolMemberPriority 设置池内一个成员的首选顺序（小者先）。固定首选/
// 顺位轮换的"首"由此决定。
func (s *Service) SetPoolMemberPriority(ctx context.Context, poolID, nodeID uint64, priority int64) error {
	store, err := s.poolStore()
	if err != nil {
		return err
	}
	members, err := store.EgressPoolMembers(ctx)
	if err != nil {
		return err
	}
	belongs := false
	for _, id := range members[poolID] {
		if id == nodeID {
			belongs = true
			break
		}
	}
	if !belongs {
		return fmt.Errorf("%w: 节点不在该池中", ErrInvalidInput)
	}
	if priority < 0 {
		return fmt.Errorf("%w: 首选顺序必须不小于 0", ErrInvalidInput)
	}
	if err := store.SetEgressPoolMemberPriority(ctx, poolID, nodeID, priority); err != nil {
		return err
	}
	s.invalidateOperationsConfig()
	s.invalidatePoolCache()
	return nil
}

func (s *Service) poolStore() (poolStore, error) {
	if s == nil || s.repository == nil {
		return nil, ErrOperationsUnavailable
	}
	store, ok := s.repository.(poolStore)
	if !ok {
		return nil, ErrOperationsUnavailable
	}
	return store, nil
}

// poolsRepository exposes the read side for save-time routing validation.
func (s *Service) poolsRepository() poolStore {
	store, _ := s.poolStore()
	return store
}

func (s *Service) publicPool(ctx context.Context, pool domain.Pool) (domain.PublicPool, error) {
	pools, err := s.ListPools(ctx)
	if err != nil {
		return domain.PublicPool{}, err
	}
	for _, public := range pools {
		if public.ID == pool.ID {
			return public, nil
		}
	}
	// 与本文件其余错误口径一致:返回应用层 ErrNotFound 而非 repository 原始
	// 错误,避免 writeError 把"池刚被并发删除"落成 500。
	return domain.PublicPool{}, ErrNotFound
}

// validatePoolInput checks name/strategy/fallback consistency and that the
// fallback chain never forms a cycle, walking the whole persisted chain: the
// runtime guard only detects cycles after traffic already degraded, so the
// configuration must reject A→B→A (and longer loops) at save time.
func (s *Service) validatePoolInput(ctx context.Context, store poolStore, input PoolInput, current *domain.Pool) error {
	if name := strings.TrimSpace(input.Name); name == "" || len(name) > 160 {
		return fmt.Errorf("%w: 池名称长度必须在 1 到 160 之间", ErrInvalidInput)
	}
	if !input.Strategy.IsValid() && input.Strategy != "" {
		return fmt.Errorf("%w: 池调度策略无效: %q", ErrInvalidInput, input.Strategy)
	}
	mode := input.FallbackMode.Normalized()
	if !mode.IsValid() {
		return fmt.Errorf("%w: 池回退模式无效: %q", ErrInvalidInput, input.FallbackMode)
	}
	if mode != domain.PoolFallbackPool {
		input.FallbackPoolID = 0
	}
	if input.FallbackPoolID == 0 {
		if mode == domain.PoolFallbackPool {
			// 数据库 CHECK(fallback_mode='pool' ⇒ fallback_pool_id>0)会兜底,
			// 但那会把本可 400 的输入错误变成 500 egressNodeOperationFailed。
			return fmt.Errorf("%w: 回退模式为 pool 时必须指定回退代理池", ErrInvalidInput)
		}
		return nil
	}
	if input.FallbackPoolID == currentID(current) {
		return fmt.Errorf("%w: 池不能回退到自身", ErrInvalidInput)
	}
	pools, err := store.ListEgressPools(ctx)
	if err != nil {
		return err
	}
	byID := make(map[uint64]domain.Pool, len(pools))
	for _, pool := range pools {
		byID[pool.ID] = pool
	}
	// 从被改的池出发沿链走:改的是本池的出边,环必然经过本池。
	next := input.FallbackPoolID
	for hop := 0; hop <= len(pools); hop++ {
		if next == currentID(current) {
			return fmt.Errorf("%w: 池回退链不能成环", ErrInvalidInput)
		}
		follower, ok := byID[next]
		if !ok || follower.FallbackMode.Normalized() != domain.PoolFallbackPool {
			return nil
		}
		next = follower.FallbackPoolID
		if next == 0 {
			return nil
		}
	}
	return nil
}

// normalizedPoolFallback zeroes the chained pool unless the mode needs it so
// the persisted row always satisfies the database CHECK.
func normalizedPoolFallback(mode domain.PoolFallbackMode, poolID uint64) uint64 {
	if mode.Normalized() != domain.PoolFallbackPool {
		return 0
	}
	return poolID
}

func currentID(current *domain.Pool) uint64 {
	if current == nil {
		return 0
	}
	return current.ID
}
