package app

import (
	"testing"
	"time"
)

// TestConsoleUsageMigrationNextRun：不完整迁移的重试节奏——有进展（仍有
// 账号成功同步）时保留 5 分钟快重试追赶进度；全败轮（succeeded==0，剩余
// 集合是死凭据/被拒账号）退回 24h 常规节奏，终止“每 5 分钟整批重试死
// 账号”的锤上游循环（历史线上排查：曾观测到持续无效流量）。
func TestConsoleUsageMigrationNextRun(t *testing.T) {
	if got := consoleUsageMigrationNextRun(5); got != consoleUsageMigrationRetry {
		t.Fatalf("progress pass next run = %s, want %s", got, consoleUsageMigrationRetry)
	}
	if got := consoleUsageMigrationNextRun(1); got != consoleUsageMigrationRetry {
		t.Fatalf("minimal progress next run = %s, want %s", got, consoleUsageMigrationRetry)
	}
	if got := consoleUsageMigrationNextRun(0); got != consoleUsageMigrationEvery {
		t.Fatalf("no-progress pass next run = %s, want %s", got, consoleUsageMigrationEvery)
	}
	if consoleUsageMigrationEvery < time.Hour {
		t.Fatalf("fallback cadence = %s, want >= 1h (storm-safe)", consoleUsageMigrationEvery)
	}
}
