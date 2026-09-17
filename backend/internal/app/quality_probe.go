package app

import (
	"context"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

// managerExitIPResolver 适配 gateway.NodeExitIPResolver/Index → egress
// Manager 的按族出口地址解析(探活通道,I8 差分前置/后置取证与排除集
// 索引)。WARP 类出口共享 v4 而 v6 各异,地址必须按族传递。
type managerExitIPResolver struct {
	Manager *infraegress.Manager
}

func (r managerExitIPResolver) NodeExitAddrs(ctx context.Context, nodeID uint64) (domainegress.ExitAddresses, error) {
	return r.Manager.ProbeNodeExitAddrs(ctx, nodeID)
}

// KnownSameExit 把出口管理器的"已知出口地址快照"适配成法院的同出口排除
// 缝:两个节点都必须在快照里有已解析的地址,且按族判等为同一出口,才回答
// true(跳过该比对候选)。快照缺失、任一侧未解析、读取出错或节点 ID 为空
// 一律回答 false(不排除)——该判定只是派发前的建议性预筛,是否可采仍以
// gateway 的活体按节点核实为唯一权威;出口 IP 随 epoch 变化,快照可能过期,
// 因此宁可多留一个候选也不误排。
func (r managerExitIPResolver) KnownSameExit(ctx context.Context, nodeIDa, nodeIDb uint64) bool {
	if nodeIDa == 0 || nodeIDb == 0 {
		return false
	}
	addrs, err := r.Manager.KnownNodeExitAddrs(ctx)
	if err != nil {
		return false
	}
	a, knownA := addrs[nodeIDa]
	b, knownB := addrs[nodeIDb]
	if !knownA || !knownB || !a.Resolved() || !b.Resolved() {
		return false
	}
	return domainegress.KnownSameEgress(a, b)
}

// runQualityProbeWorker gives the investigator the application lifetime.
func (a *Application) runQualityProbeWorker(ctx context.Context) error {
	if a.qualityInvestigator == nil || a.qualityProbeExec == nil {
		<-ctx.Done()
		return nil
	}
	return a.qualityInvestigator.Run(ctx, a.qualityProbeExec, a.logger)
}
