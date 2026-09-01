package middleware

import (
	"log/slog"
	"testing"
	"time"
)

// slowLogSink 模拟慢读端(docker logs / journald 管道背压):每次写阻塞 1ms。
type slowLogSink struct{ delay time.Duration }

func (s slowLogSink) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	return len(p), nil
}

// BenchmarkAccessLogSlowSinkBackpressure 判别性基准:慢读端(1ms/写)下,
// 同步路径的 write 在 slog 互斥锁内阻塞——所有并行请求随之串行停摆
// (生产形态:日志采集端卡顿会拖住全部请求处理);异步路径请求侧不被
// 慢写连坐(队列吸收,超深丢弃)。
func BenchmarkAccessLogSlowSinkBackpressure(b *testing.B) {
	sink := slowLogSink{delay: time.Millisecond}
	syncLogger := slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	asyncWriter := newAsyncAccessLogWriterWithSink(sink)
	b.Cleanup(func() { _ = asyncWriter.Close() })
	asyncLogger := slog.New(slog.NewJSONHandler(asyncWriter, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b.Run("sync-slow-sink", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				syncLogger.Info("http_request", "request_id", "req", "status", 200)
			}
		})
	})
	b.Run("async-slow-sink", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				asyncLogger.Info("http_request", "request_id", "req", "status", 200)
			}
		})
	})
}
