package middleware

import (
	"log/slog"
	"os"
	"testing"
)

// BenchmarkAccessLogSyncVsAsync 量化并发下访问日志写路径的争用差异。
// sink 用 /dev/null:保留真实 write 系统调用(与 stdout 同构),仅内核侧
// 丢弃——io.Discard 会把 syscall 成本完全藏掉,测出的只有编码差异。
func BenchmarkAccessLogSyncVsAsync(b *testing.B) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = devnull.Close() })
	syncLogger := slog.New(slog.NewJSONHandler(devnull, &slog.HandlerOptions{Level: slog.LevelInfo}))
	asyncWriter := newAsyncAccessLogWriterWithSink(devnull)
	b.Cleanup(func() { _ = asyncWriter.Close() })
	asyncLogger := slog.New(slog.NewJSONHandler(asyncWriter, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b.Run("sync-json", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				syncLogger.Info("http_request", "request_id", "req", "method", "GET", "path", "/v1/x", "status", 200, "duration_ms", 5)
			}
		})
	})
	b.Run("async-queue", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				asyncLogger.Info("http_request", "request_id", "req", "method", "GET", "path", "/v1/x", "status", 200, "duration_ms", 5)
			}
		})
	})
}
