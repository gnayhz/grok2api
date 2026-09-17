// Package proxy 是代理网络在质量层的观测面:节点三型画像与拨号选择分布。
// 池策略与路由分层统计的生产实现在 infra/egress——本包不平行实现选路。
package proxy

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// NodeProfile 是节点的代理网络画像(从底座数据派生,只读)。
type NodeProfile struct {
	ID                  uint64
	Enabled             bool
	Name                string
	ProxyPool           bool
	RotationWebhook     bool
	CanServeFixedTarget bool
	CooldownUntil       *time.Time
	// PoolSticky 池隧道粘性子型。当前 NodeFacts 没有独立子型字段,
	// 组合根把所有池节点标成 sticky;按请求池每次探测见新 IP,
	// epoch 翻篇同样释放,与粘性池在质量处置上等价。
	PoolSticky bool
}

// Type 返回节点三型分类。
func (n NodeProfile) Type() model.NodeType {
	switch {
	case !n.ProxyPool && n.RotationWebhook:
		return model.NodeWebhook
	case !n.ProxyPool:
		return model.NodeFixed
	case n.PoolSticky:
		return model.NodePoolSticky
	default:
		return model.NodePoolPerRequest
	}
}

// NodeSource supplies current transport facts. Failure is distinct from a
// missing node; callers must not reuse old observations as current eligibility.
type NodeSource interface {
	ListProfiles(ctx context.Context) ([]NodeProfile, error)
	Profile(ctx context.Context, nodeID uint64) (NodeProfile, bool, error)
}
