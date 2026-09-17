package app

import (
	"context"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
	egresshttp "github.com/chenyme/grok2api/backend/internal/transport/http/egress"
)

// 底座缝隙的质量层实现适配器(重写批2,D3):组合根把质量层接到
// 底座声明的缝隙接口上。适配器只做翻译,不含判定逻辑——判定归
// 质量层,概念不泄漏进底座(B4 依赖铁律)。

// qualityAccountEligibility exposes a selection hint and the independent
// persistent admission check; neither can change account-owned health policy.
type qualityAccountEligibility struct {
	registry *qualityregistry.Registry
	journal  *journal.Store
}

func (a qualityAccountEligibility) AccountSchedulable(accountID uint64) bool {
	return a.registry.AccountEligible(accountID)
}

func (a qualityAccountEligibility) CheckAccountAdmission(ctx context.Context, id uint64) (bool, error) {
	if a.journal == nil {
		return a.registry.AccountEligible(id), nil
	}
	return a.journal.AccountAllowed(ctx, id, time.Now().UTC())
}

// qualityExitEligibility 实现 egress.ExitEligibility(D3-1)。
type qualityExitEligibility struct {
	registry *qualityregistry.Registry
}

func (a qualityExitEligibility) ExitSchedulable(nodeID uint64) bool {
	return a.registry.ExitEligible(nodeID)
}

func (a qualityExitEligibility) CheckExitAdmission(ctx context.Context, nodeID uint64) (bool, error) {
	return a.registry.ExitAllowed(ctx, nodeID)
}

// egressQualityStatesProvider 适配节点列表质量徽章注入(批8 可见性
// 整改):登记处当前 epoch 出口状态 → 管理面节点响应投影。底座只
// 透传展示,不理解羁押语义(B4 依赖铁律:缝隙在底座侧,实现在质量层)。
type egressQualityStatesProvider struct {
	registry *qualityregistry.Registry
}

func (p egressQualityStatesProvider) states() map[uint64]egresshttp.NodeQualityState {
	entries := p.registry.CurrentExitStates()
	states := make(map[uint64]egresshttp.NodeQualityState, len(entries))
	for nodeID, entry := range entries {
		states[nodeID] = egresshttp.NodeQualityState{
			State: string(entry.State), CaseID: entry.CurrentCaseID,
		}
	}
	return states
}

// accountQualityStatesProvider adapts the current quality revision for account
// management queries. Filtering and badges consume the same read projection.
type accountQualityStatesProvider struct {
	registry *qualityregistry.Registry
}

func (p accountQualityStatesProvider) states(ctx context.Context) (map[uint64]accountapp.QualityState, error) {
	projection, err := p.registry.ManagementState(ctx)
	if err != nil {
		return nil, err
	}
	states := make(map[uint64]accountapp.QualityState, len(projection.Accounts))
	for id, entry := range projection.Accounts {
		states[id] = accountapp.QualityState{State: string(entry.State), CaseID: entry.CurrentCaseID}
	}
	return states, nil
}
