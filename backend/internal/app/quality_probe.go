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

// KnownNodeExitAddrs 出口已知地址索引(节点快照,探活维护):差分排除
// 集据此按族判等,只排除真孪生节点。
func (r managerExitIPResolver) KnownNodeExitAddrs(ctx context.Context) (map[uint64]domainegress.ExitAddresses, error) {
	return r.Manager.KnownNodeExitAddrs(ctx)
}

// runQualityProbeWorker gives the investigator the application lifetime.
func (a *Application) runQualityProbeWorker(ctx context.Context) error {
	if a.qualityInvestigator == nil || a.qualityProbeExec == nil {
		<-ctx.Done()
		return nil
	}
	return a.qualityInvestigator.Run(ctx, a.qualityProbeExec, a.logger)
}
