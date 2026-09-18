package upstreamtrace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// upstreamtrace 是真实上游形态采样的观测工具（用户批准的全链路摸底）。
// GROK2API_UPSTREAM_TRACE_DIR 保存原始请求/响应及网络阶段；独立设置
// GROK2API_UPSTREAM_NETWORK_TRACE_DIR 可只保存不含内容的网络阶段。
// 文件名: <unixms>_<seq>_<op>_<model>_<kind>.<ext>。
// Build（cli）与 Console 两个受守卫的通道共用。

var (
	dir        atomic.Value
	networkDir atomic.Value
	seq        atomic.Uint64
	once       sync.Once
)

func Enabled() (string, bool) {
	once.Do(func() {
		raw, network := traceDirectories()
		dir.Store(raw)
		networkDir.Store(network)
	})
	d, _ := dir.Load().(string)
	return d, d != ""
}

func traceDirectories() (raw, network string) {
	prepare := func(path string) string {
		if path != "" && os.MkdirAll(path, 0o700) == nil {
			return path
		}
		return ""
	}
	raw = prepare(os.Getenv("GROK2API_UPSTREAM_TRACE_DIR"))
	network = raw
	if path := os.Getenv("GROK2API_UPSTREAM_NETWORK_TRACE_DIR"); path != "" {
		network = prepare(path)
	}
	return raw, network
}

func path(d, op, model, kind, ext string) string {
	n := seq.Add(1)
	return filepath.Join(d, fmt.Sprintf("%d_%03d_%s_%s_%s.%s", time.Now().UnixMilli(), n%1000, op, model, kind, ext))
}

// DumpRequest 落盘归一化后的上游请求（形态-参数关联用）。
func DumpRequest(d, op, model string, streaming bool, body []byte) {
	kind := "body"
	if streaming {
		kind = "stream"
	}
	_ = os.WriteFile(path(d, op, model, kind+".req", "json"), body, 0o600)
}

// TeeStream 在原始上游 SSE 流上叠加落盘（转换器之前），分片前写 #ts 毫秒
// 时间戳标记（非 data 行，不污染解析）——轨迹自带到达时序。
func TeeStream(d, op, model string, body io.ReadCloser) io.ReadCloser {
	f, err := os.OpenFile(path(d, op, model, "stream", "sse"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return body // 采样失败不影响请求路径
	}
	return &teeReadCloser{rc: body, f: f}
}

type teeReadCloser struct {
	rc io.ReadCloser
	f  *os.File
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		_, _ = fmt.Fprintf(t.f, "#ts %d\n", time.Now().UnixMilli())
		_, _ = t.f.Write(p[:n]) // 采样写失败静默：观测工具不得影响请求
	}
	return n, err
}

func (t *teeReadCloser) Close() error {
	err := t.rc.Close()
	_ = t.f.Close()
	return err
}

// TeeBody 包装非流式原始响应：读取过程镜像到缓冲，EOF 时整体落盘
// （native responses 非流式路径 body 直通不读，用包装捕获）。
func TeeBody(d, op, model string, body io.ReadCloser) io.ReadCloser {
	f, err := os.OpenFile(path(d, op, model, "body", "json"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return body
	}
	return &bodyTeeReadCloser{rc: body, f: f}
}

type bodyTeeReadCloser struct {
	rc io.ReadCloser
	f  *os.File
}

func (t *bodyTeeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		_, _ = t.f.Write(p[:n])
	}
	if err == io.EOF {
		_ = t.f.Close()
	}
	return n, err
}

func (t *bodyTeeReadCloser) Close() error {
	err := t.rc.Close()
	_ = t.f.Close()
	return err
}
