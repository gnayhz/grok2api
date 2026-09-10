package proxy

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"time"
)

// Package proxy 是代理网络在质量层的观测面(G10 数据面):节点三型画像
// (B4)与拨号选择分布。池策略(含 G9 最少使用)与路由分层统计(G11)
// 的生产实现在底座 infra/egress——质量层不再保留平行实现。

// NodeProfile 是节点的代理网络画像(从底座数据派生,只读)。
type NodeProfile struct {
	ID                  uint64
	Enabled             bool
	Name                string
	ProxyPool           bool
	RotationWebhook     bool
	CanServeFixedTarget bool
	CooldownUntil       *time.Time
	// PoolSticky 池隧道粘性子型(供应商规定时间内固定);按请求子型
	// 数据面暂无区分字段,统一按粘性处理——按请求池每次探测必见新
	// IP,epoch 翻篇自动释放,行为等价(批4 决议,见进度档)。
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
