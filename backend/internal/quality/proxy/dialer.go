package proxy

import (
	"sync"
)

// DialerPolicy 拨号观测面:每次出口租约按作用域×节点计数(G10 选择
// 分布数据面)。生产路由/池决策在底座 Manager——本包零底座 import
// (D2),组合根把本观测包装到底座 Dialer 缝隙上。
type DialerPolicy struct {
	mu       sync.Mutex
	perScope map[string]map[uint64]uint64
}

// NewDialerPolicy 构建拨号观测。
func NewDialerPolicy() *DialerPolicy {
	return &DialerPolicy{perScope: map[string]map[uint64]uint64{}}
}

// ObserveAcquisition 记录一次出口租约获取(选择分布数据面)。
func (p *DialerPolicy) ObserveAcquisition(scope string, nodeID uint64) {
	if p == nil || nodeID == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.perScope[scope] == nil {
		p.perScope[scope] = map[uint64]uint64{}
	}
	p.perScope[scope][nodeID]++
}

// SelectionDistribution 返回各作用域各节点的累计选择次数(G10 面板)。
func (p *DialerPolicy) SelectionDistribution() map[string]map[uint64]uint64 {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := make(map[string]map[uint64]uint64, len(p.perScope))
	for scope, nodes := range p.perScope {
		copied := make(map[uint64]uint64, len(nodes))
		for nodeID, count := range nodes {
			copied[nodeID] = count
		}
		snapshot[scope] = copied
	}
	return snapshot
}
