package guard

import guardpolicy "github.com/chenyme/grok2api/backend/internal/domain/guard"

// 本文件是登记在 test_seams_test.go 冻结清单中的跨包测试接缝:
// DefaultConfig/New 只服务其它包的契约测试与纯内存(可剥离)形态,
// 生产组合根使用 NewWithFileDefaults(文件基线+持久化合并)。
// 两个函数均为单语句转发,不持状态、不含策略。

// DefaultConfig 暴露域默认策略(契约测试与纯内存形态使用;生产启动
// 经组合根的文件基线+持久化合并,不直接调用)。
func DefaultConfig() Config { return guardpolicy.DefaultConfig() }

// New 构建守卫配置服务;store 可为 nil(纯内存,测试/剥离形态)。
// 生产组合根使用 NewWithFileDefaults(区分启动兜底与文件基线)。
func New(cfg Config, store Store) *Service {
	return NewWithFileDefaults(cfg, cfg, store)
}
