package egress

import (
	"context"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 底座缝隙接口(D3/B4 决议2):定义在底座侧,质量层实现,组合根注入。
// 未注入(nil)时底座按内建行为运行——可剥离性(D2)由此保证。

// ExitEligibility 是出口资格谓词缝隙(D3-1/B2):出口路由与池成员选择
// 经同一谓词问"此出口可调度吗"。质量轴概念(ban/羁押)不泄漏进底座;
// 健康轴(探活/软冷却)仍归底座,两轴独立判定后 AND。
// 实现必须 O(1) 内存可判定。
type ExitEligibility interface {
	ExitSchedulable(nodeID uint64) bool
}

// ExitAdmission is the durable final check, after cached candidate selection.
// Storage failures must fail admission rather than fall back to stale state.
type ExitAdmission interface {
	CheckExitAdmission(context.Context, uint64) (bool, error)
}

// Dialer 是拨号器缝隙(D3-2):转发循环消费的出口拨号面。底座自带直连
// 拨号(未配置路由时的 fallback transport);质量层注入代理路由拨号器
// (批4 proxy 包)。Manager 满足本接口(下方编译期断言)。
type Dialer interface {
	// AcquireIfConfigured 按路由配置取出口租约;未配置时返回
	// configured=false,由调用方走自带直连。
	AcquireIfConfigured(ctx context.Context, scope domainegress.Scope, affinity string) (*Lease, bool, error)
	// AcquireBuildEnvironmentDirectIfIsolated 账号隔离直连(Build 环境)。
	AcquireBuildEnvironmentDirectIfIsolated(ctx context.Context, affinity string) (*Lease, bool, error)
	// AcquireBuildEnvironmentDirect manages the default Build transport.
	AcquireBuildEnvironmentDirect(ctx context.Context, affinity string) (*Lease, error)
	// FeedbackForScope 回写出口健康反馈(传输轴)。
	FeedbackForScope(ctx context.Context, scope domainegress.Scope, nodeID uint64, statusCode int, err error)
	// BuildStreamIdleTimeout 流空闲超时配置。
	BuildStreamIdleTimeout() time.Duration
}

var _ Dialer = (*Manager)(nil)

// exitEligibilityBypassKey 标记取证/验证上下文:质量资格缝隙对探针
// 通道放行(B2 生产/探针双通道——证据引擎能工作的前提)。
type exitEligibilityBypassKey struct{}

// WithExitEligibilityBypass 标记 ctx 为质量取证通道:出口资格谓词放行。
// 调查局探针(批3)与质量验证钉住使用。
func WithExitEligibilityBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, exitEligibilityBypassKey{}, true)
}

func exitEligibilityBypassed(ctx context.Context) bool {
	value, ok := ctx.Value(exitEligibilityBypassKey{}).(bool)
	return ok && value
}
