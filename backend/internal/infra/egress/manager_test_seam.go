package egress

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 本文件是登记在 internal/architecture/test_seams_test.go 冻结清单中的
// 跨包测试接缝:Manager.Acquire 是基准/压力/live 测试的统一拨号入口,
// 生产消费方走 AcquireCredential / AcquireIfConfigured /
// AcquireBuildEnvironmentDirect*;单语句转发,不持状态、不含策略。
func (m *Manager) Acquire(ctx context.Context, scope egress.Scope, affinity string) (*Lease, error) {
	lease, _, err := m.acquire(ctx, scope, affinity, true, "")
	return lease, err
}
