package gateway

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingReadCloser 记录 Close 是否被调用, 用于验证重放 Reader 会把
// Close 传导给底层 body。
type countingReadCloser struct {
	Reader io.Reader
	closed *int32
}

func (c *countingReadCloser) Read(p []byte) (int, error) { return c.Reader.Read(p) }
func (c *countingReadCloser) Close() error {
	atomic.StoreInt32(c.closed, 1)
	return nil
}

// TestPeekPathsDoNotLeakGoroutines 是长时运行稳定性维度的回归锁：每个
// peekQualityStream/peekQualityBody 返回路径（判决/超时/取消/异常）都会
// 派生一个 qualityReadPump goroutine；任何路径漏掉 Close 都会按请求泄漏。
// 每路径迭代 N 次后 goroutine 数必须回落到基线。带后台生产者的场景先
// 用 WaitGroup drain 等待 harness 自身协程退出（首轮实现曾把仍在
// time.Sleep 的测试生产者误判为泄漏——测量必须排除测量仪器本身）。
func TestPeekPathsDoNotLeakGoroutines(t *testing.T) {
	const iterations = 50
	cfg := QualityRetryRuntime{
		Enabled:         true,
		EvidenceTimeout: 80 * time.Millisecond,
		CreatedTimeout:  60 * time.Millisecond,
	}

	stableWindow := []int{runtime.NumGoroutine(), runtime.NumGoroutine()}
	lastTwoStable := func(current int) int {
		stableWindow[0], stableWindow[1] = stableWindow[1], current
		return stableWindow[0]
	}
	settled := func() int {
		deadline := time.Now().Add(5 * time.Second)
		best := runtime.NumGoroutine()
		for time.Now().Before(deadline) {
			runtime.Gosched()
			time.Sleep(5 * time.Millisecond)
			current := runtime.NumGoroutine()
			if current <= best && current == lastTwoStable(current) {
				return current
			}
			best = min(best, current)
		}
		return best
	}
	_ = settled() // 预热稳定窗
	baseline := settled()

	slowProducer := func(wg *sync.WaitGroup) io.ReadCloser {
		r, w := io.Pipe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = w.Close() }()
			_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"h\"}\n\n"))
			for i := 0; i < iterations*4; i++ {
				time.Sleep(2 * time.Millisecond)
				if _, err := w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"word\"}\n\n")); err != nil {
					return
				}
			}
		}()
		return r
	}

	healthy := func() io.ReadCloser {
		var b strings.Builder
		b.WriteString("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"think\"}\n\n")
		b.WriteString("data: {\"type\":\"response.completed\"}\n\n")
		b.WriteString("data: [DONE]\n\n")
		return io.NopCloser(strings.NewReader(b.String()))
	}
	withholdStream := func() io.ReadCloser {
		var b strings.Builder
		b.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"word word word word\"}\n\n")
		b.WriteString("data: {\"type\":\"response.completed\"}\n\n")
		return io.NopCloser(strings.NewReader(b.String()))
	}

	scenarios := []struct {
		name  string
		run   func(i int)
		drain func()
	}{
		{"deliver drained", func(int) {
			replay, _, _, _ := peekQualityStream(context.Background(), healthy(), qualityProtocolResponses, cfg)
			_, _ = io.Copy(io.Discard, replay)
			_ = replay.Close()
		}, nil},
		{"deliver client abandons mid-stream", func(int) {}, nil}, // 占位，下方覆写
		{"withhold caller discards unread", func(int) {
			replay, verdict, _, _ := peekQualityStream(context.Background(), withholdStream(), qualityProtocolResponses, cfg)
			if verdict != QualityWithhold {
				t.Errorf("withhold scenario verdict = %s", verdict)
			}
			_ = replay.Close() // 生产侧丢弃：service.go 扣留路径的行为
		}, nil},
		{"empty stream error path", func(int) {
			replay, _, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n")), qualityProtocolResponses, cfg)
			if err == nil {
				t.Error("empty stream must yield error")
			}
			_ = replay.Close()
		}, nil},
	}

	// 带后台生产者的三个场景：drain 等待 harness 协程退出后再测量。
	var wg sync.WaitGroup
	abandon := func(int) {
		replay, _, _, _ := peekQualityStream(context.Background(), slowProducer(&wg), qualityProtocolResponses, cfg)
		_ = replay.Close() // 不读完即放弃
	}
	createdTimeout := func(int) {
		r, w := io.Pipe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte("data: {}\n\n"))
			_ = w.Close()
		}()
		replay, _, _, err := peekQualityStream(context.Background(), r, qualityProtocolResponses, cfg)
		if err == nil {
			t.Error("created timeout must yield error")
		}
		_ = replay.Close()
		_ = w.Close()
	}
	evidenceTimeout := func(int) {
		r, w := io.Pipe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\"}\n\n")) // 有事件但无证据无输出
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
			_ = w.Close()
		}()
		replay, _, _, err := peekQualityStream(context.Background(), r, qualityProtocolResponses, cfg)
		if err == nil {
			t.Error("evidence timeout must yield error")
		}
		_ = replay.Close()
		_ = w.Close()
	}
	ctxCancel := func(int) {
		ctx, cancel := context.WithCancel(context.Background())
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		r, w := io.Pipe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(300 * time.Millisecond)
			_ = w.Close()
		}()
		replay, _, _, _ := peekQualityStream(ctx, r, qualityProtocolResponses, cfg)
		_ = replay.Close()
		_ = w.Close()
		cancel()
	}
	drainProducers := func() { wg.Wait() }

	scenarios[1].run = abandon
	scenarios[1].drain = drainProducers
	scenarios = append(scenarios,
		struct {
			name  string
			run   func(i int)
			drain func()
		}{"created timeout aborts", createdTimeout, drainProducers},
		struct {
			name  string
			run   func(i int)
			drain func()
		}{"evidence timeout aborts", evidenceTimeout, drainProducers},
		struct {
			name  string
			run   func(i int)
			drain func()
		}{"context cancel aborts", ctxCancel, drainProducers},
		struct {
			name  string
			run   func(i int)
			drain func()
		}{"body classifier roundtrip", func(int) {
			body := io.NopCloser(strings.NewReader(
				"{\"output\":[{\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"t\"}]},{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}"))
			replay, verdict, _, err := peekQualityBody(body, cfg)
			if err != nil || verdict != QualityDeliver {
				t.Errorf("body deliver verdict=%s err=%v", verdict, err)
			}
			_, _ = io.Copy(io.Discard, replay)
			_ = replay.Close()
		}, nil},
		struct {
			name  string
			run   func(i int)
			drain func()
		}{"oversized body closes underlying body", func(int) {
			var closed int32
			raw := &countingReadCloser{Reader: bytes.NewReader(make([]byte, qualityBodyPeekLimit+1)), closed: &closed}
			replay, verdict, _, err := peekQualityBody(raw, cfg)
			if err != nil || verdict != QualityDeliver {
				t.Errorf("oversized verdict=%s err=%v", verdict, err)
			}
			if replay == nil {
				t.Fatal("replay is nil")
			}
			if err := replay.Close(); err != nil {
				t.Errorf("replay close: %v", err)
			}
			if atomic.LoadInt32(&closed) != 1 {
				t.Fatalf("underlying body not closed by replay.Close (closed=%d): upstream connection and egress lease leak", closed)
			}
		}, nil},
	)

	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario.name, func(t *testing.T) {
			for i := 0; i < iterations; i++ {
				scenario.run(i)
			}
			if scenario.drain != nil {
				scenario.drain()
			}
			after := settled()
			if after > baseline+2 {
				buf := make([]byte, 1<<16)
				n := runtime.Stack(buf, true)
				t.Fatalf("goroutine leak: baseline=%d after=%d (+%d)\n%s", baseline, after, after-baseline, buf[:n])
			}
		})
	}
}
