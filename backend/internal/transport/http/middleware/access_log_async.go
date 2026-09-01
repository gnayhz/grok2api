package middleware

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	asyncAccessLogQueueDepth   = 8192
	asyncAccessLogBufferBytes  = 64 << 10
	asyncAccessLogFlushBytes   = 32 << 10
	asyncAccessLogFlushPeriod  = 200 * time.Millisecond
	asyncAccessLogDrainTimeout = 2 * time.Second
)

// asyncAccessLogWriter 把每请求一条的访问日志从同步 stdout 写改造为
// 有界队列 + 后台批量刷写:此前每条日志 = slog JSONHandler 内部互斥锁
// (全局串行点)+ 一次无缓冲 write 系统调用,高 QPS 下所有请求在日志上
// 排队。slog JSONHandler 每条记录恰好一次 Write(整行含换行),因此按行
// 入队;队列满时丢弃并计数(与失效总线同款采样告警),日志顺序保持 FIFO。
type asyncAccessLogWriter struct {
	lines     chan []byte
	out       io.Writer
	closed    chan struct{}
	drained   chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
}

func newAsyncAccessLogWriter() *asyncAccessLogWriter {
	return newAsyncAccessLogWriterWithSink(os.Stdout)
}

func newAsyncAccessLogWriterWithSink(out io.Writer) *asyncAccessLogWriter {
	w := &asyncAccessLogWriter{lines: make(chan []byte, asyncAccessLogQueueDepth), closed: make(chan struct{}), drained: make(chan struct{}), out: out}
	go w.run()
	return w
}

func (w *asyncAccessLogWriter) run() {
	defer close(w.drained)
	out := bufio.NewWriterSize(w.out, asyncAccessLogBufferBytes)
	ticker := time.NewTicker(asyncAccessLogFlushPeriod)
	defer ticker.Stop()
	for {
		select {
		case line := <-w.lines:
			_, _ = out.Write(line)
			// 批量收割:非阻塞排空已就绪的行,一次 syscall 冲刷整批。
		drain:
			for out.Buffered() < asyncAccessLogFlushBytes {
				select {
				case more := <-w.lines:
					_, _ = out.Write(more)
				default:
					break drain
				}
			}
			_ = out.Flush()
		case <-ticker.C:
			if out.Buffered() > 0 {
				_ = out.Flush()
			}
		case <-w.closed:
			for {
				select {
				case line := <-w.lines:
					_, _ = out.Write(line)
				default:
					_ = out.Flush()
					return
				}
			}
		}
	}
}

func (w *asyncAccessLogWriter) Write(p []byte) (int, error) {
	// slog 可能复用传入缓冲,入队前必须拷贝。
	line := make([]byte, len(p))
	copy(line, p)
	select {
	case w.lines <- line:
	default:
		dropped := w.dropped.Add(1)
		if dropped == 1 || dropped%1000 == 0 {
			fmt.Fprintf(os.Stderr, "access_log_queue_full dropped=%d\n", dropped)
		}
	}
	return len(p), nil
}

func (w *asyncAccessLogWriter) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	select {
	case <-w.drained:
	case <-time.After(asyncAccessLogDrainTimeout):
	}
	return nil
}

var (
	asyncAccessLoggerOnce   sync.Once
	asyncAccessLoggerHolder *slog.Logger
	asyncAccessWriterHolder *asyncAccessLogWriter
)

// AsyncAccessLogger 返回访问日志专用异步 logger(进程级单例):JSON 格式
// 与默认 logger 完全一致,仅写出路径改为有界队列 + 批量刷写。注入自定义
// logger 的测试继续走 AccessLog(logger) 的同步语义,行为不受影响。
func AsyncAccessLogger() *slog.Logger {
	asyncAccessLoggerOnce.Do(func() {
		asyncAccessWriterHolder = newAsyncAccessLogWriter()
		asyncAccessLoggerHolder = slog.New(slog.NewJSONHandler(asyncAccessWriterHolder, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	return asyncAccessLoggerHolder
}

// FlushAsyncAccessLogs 优雅关停时冲刷缓冲中的访问日志;超时兜底防卡死。
func FlushAsyncAccessLogs() {
	if asyncAccessWriterHolder != nil {
		_ = asyncAccessWriterHolder.Close()
	}
}

var _ io.Writer = (*asyncAccessLogWriter)(nil)
