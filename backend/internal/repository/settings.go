package repository

import (
	"context"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

// RuntimeSettingsRepository 定义运行设置的单实例持久化边界。
type RuntimeSettingsRepository interface {
	Get(ctx context.Context) (settingsdomain.Config, time.Time, uint64, bool, error)
	Save(ctx context.Context, value settingsdomain.Config, expectedRevision uint64) (time.Time, uint64, error)
	// Delete 移除持久化运行设置行，恢复「以配置文件为默认」的优先级
	// 语义；行不存在时按成功处理（幂等）。
	Delete(ctx context.Context) error
}
