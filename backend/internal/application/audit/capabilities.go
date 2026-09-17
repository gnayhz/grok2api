package audit

import (
	"context"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// Queries 是审计只读查询能力：列表、游标检索、明细与汇总。消费方（HTTP
// 查询、仪表盘）只拿到本合同，不接触写入、账本健康或生命周期。
type Queries interface {
	List(ctx context.Context, page, pageSize int) ([]auditdomain.Record, int64, error)
	ListCursor(ctx context.Context, rawCursor string, pageSize int, search, rawPeriod string, filter ListFilter) (CursorResult, error)
	Get(ctx context.Context, id uint64) (auditdomain.Record, error)
	Summary(ctx context.Context, search, rawPeriod string, filter ListFilter) (SummaryResult, error)
	SummaryFresh(ctx context.Context, search, rawPeriod string, filter ListFilter) (SummaryResult, error)
}

// Writer 是持久接收能力：本地 durable 接受与最终 SQL 提交分离；取消不
// 丢弃已接受费用。执行路径只依赖本合同。
type Writer interface {
	Create(ctx context.Context, value auditdomain.Record) error
}

// WriterConfig 允许运行设置热更新写入批次参数（App 装配的目标之一）。
type WriterConfig interface {
	UpdateWriterConfig(batchSize int, flushInterval, commitDelay time.Duration)
}

// LedgerHealth 是账本健康与结算观测能力：快照与就绪检查。执行入口读取
// 快照，不由 gateway 触碰内部队列。
type LedgerHealth interface {
	LedgerSnapshot() LedgerSnapshot
	CheckLedgerReady() error
}

// 能力断言：同一个实现（*Service）同时满足各消费面，但消费方按合同
// 依赖，静态上无法越权调用其他面的方法。
var (
	_ Queries      = (*Service)(nil)
	_ Writer       = (*Service)(nil)
	_ WriterConfig = (*Service)(nil)
	_ LedgerHealth = (*Service)(nil)
)
