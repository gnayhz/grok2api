package registry

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"

	"gorm.io/gorm"
)

// AccountLinks supplies associations from their owner. Quality decides how
// those facts form investigation identity groups; it never reads account SQL.
// Implementations must return a coherent snapshot or an error. An unavailable
// source is not an empty fleet.
type AccountLinks interface {
	ListIdentityLinks(context.Context) ([]account.IdentityLink, error)
}

// computeIdentityGroups 用并查集把关联边聚成连通分量;
// 只保留多成员组,成员排序后 FNV-64a 作为确定性组号(重启稳定)。
func computeIdentityGroups(edges []account.IdentityLink) map[uint64][]uint64 {
	parent := map[uint64]uint64{}
	var find func(uint64) uint64
	find = func(x uint64) uint64 {
		root, ok := parent[x]
		if !ok || root == x {
			parent[x] = x
			return x
		}
		root = find(root)
		parent[x] = root
		return root
	}
	union := func(a, b uint64) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for _, edge := range edges {
		union(edge.AccountID, edge.RelatedAccountID)
	}
	groups := map[uint64][]uint64{}
	for member := range parent {
		root := find(member)
		groups[root] = append(groups[root], member)
	}
	result := map[uint64][]uint64{}
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
		result[identityGroupID(members)] = members
	}
	return result
}

// identityGroupID 成员集合的确定性标识:FNV-64a over sorted IDs。
// 成员集合不变则组号不变;集合演变时自然换号(新组=新命运)。
// 高位掩蔽:SQLite INTEGER 是有符号 64 位,组号必须落在正区间。
func identityGroupID(members []uint64) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	sorted := append([]uint64(nil), members...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	hash := uint64(offset64)
	for _, member := range sorted {
		for shift := 0; shift < 64; shift += 8 {
			hash ^= (member >> shift) & 0xff
			hash *= prime64
		}
	}
	hash &= (1 << 63) - 1
	if hash == 0 {
		hash = 1
	}
	return hash
}

// RefreshIdentityGroups 重算身份组并持久化到 q_identity_group
// (整表重写:组号确定性,幂等)。账号关联变化后由调用方触发;
// 启动时 Open 已重建。
func (r *Registry) RefreshIdentityGroups(ctx context.Context) error {
	// A standalone quality store preserves the last projection. Only an
	// explicitly wired source may replace it, including with an empty set.
	if r.accountLinks == nil {
		return ctx.Err()
	}
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.RefreshIdentityGroups(ctx) })
	}
	edges, err := r.accountLinks.ListIdentityLinks(ctx)
	if err != nil {
		return fmt.Errorf("read account identity links: %w", err)
	}
	groups := computeIdentityGroups(edges)
	r.transitionMu <- struct{}{}
	defer func() { <-r.transitionMu }()

	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM q_identity_group").Error; err != nil {
			return err
		}
		if len(groups) == 0 {
			return nil
		}
		now := time.Now().UTC()
		rows := make([]qIdentityGroupModel, 0, len(groups))
		for groupID, members := range groups {
			for _, member := range members {
				rows = append(rows, qIdentityGroupModel{GroupID: groupID, AccountID: member, AddedAt: now})
			}
		}
		return tx.Create(&rows).Error
	}); err != nil {
		return err
	}
	snap := r.snapshot.load()
	next := snap.clone()
	next.groupOf = map[uint64]uint64{}
	next.groupMembers = map[uint64][]uint64{}
	for groupID, members := range groups {
		for _, member := range members {
			next.groupOf[member] = groupID
		}
		next.groupMembers[groupID] = members
	}
	r.snapshot.store(next)
	return nil
}

// IdentityGroupOf 无锁读取账号所属身份组，并返回成员副本。
// 未关联账号自成一组:组号=自身,成员=自身。
func (r *Registry) IdentityGroupOf(accountID uint64) (groupID uint64, members []uint64) {
	snap := r.snapshot.load()
	if id, ok := snap.groupOf[accountID]; ok {
		if group := snap.groupMembers[id]; len(group) > 0 {
			return id, append([]uint64(nil), group...)
		}
	}
	return accountID, []uint64{accountID}
}
