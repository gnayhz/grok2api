package streampipe

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// streampipe 是流式热路径的 panic 隔离边界（4 个 SSE 转换调用点共用），
// 此前零测试。以下锁定全部契约语义。

func TestRunClosesWriterWithNilOnSuccess(t *testing.T) {
	reader, writer := io.Pipe()
	go Run(writer, func() error { return nil })
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reader must see clean EOF, got %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected no bytes, got %q", data)
	}
}

func TestRunPropagatesWorkError(t *testing.T) {
	reader, writer := io.Pipe()
	cause := errors.New("upstream reset")
	go Run(writer, func() error { return cause })
	_, err := io.ReadAll(reader)
	if !errors.Is(err, cause) {
		t.Fatalf("reader must see the work error, got %v", err)
	}
}

func TestRunContainsPanicAndReportsPanicError(t *testing.T) {
	reader, writer := io.Pipe()
	go Run(writer, func() error { panic("converter exploded") })
	_, err := io.ReadAll(reader)
	var panicErr *PanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("reader must see PanicError, got %T %v", err, err)
	}
	if panicErr.Value != "converter exploded" {
		t.Fatalf("panic value = %v", panicErr.Value)
	}
	if !strings.Contains(string(panicErr.Stack), "TestRunContainsPanicAndReportsPanicError") {
		t.Fatalf("stack must capture the panicking call site, got: %.200s", panicErr.Stack)
	}
	if !strings.Contains(panicErr.Error(), "converter exploded") {
		t.Fatalf("Error() must mention the panic value: %s", panicErr.Error())
	}
}

// 运行方在 panic 后必须继续可服务（进程存活语义的直接近似）。
func TestRunSurvivesPanicForSubsequentCalls(t *testing.T) {
	for i := 0; i < 3; i++ {
		reader, writer := io.Pipe()
		go Run(writer, func() error { panic(i) })
		if _, err := io.ReadAll(reader); err == nil {
			t.Fatal("expected propagated panic error")
		}
	}
	reader, writer := io.Pipe()
	go Run(writer, func() error { return nil })
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("Run must remain usable after panics: %v", err)
	}
}

// 语义守护：干净写入路径必须以 EOF 结束（消费侧以 err==nil 判定正常）。
func TestRunNilErrorIsCleanEOF(t *testing.T) {
	reader, writer := io.Pipe()
	go Run(writer, func() error {
		_, writeErr := writer.Write([]byte("chunk"))
		return writeErr
	})
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("clean write path must end in EOF, got %v", err)
	}
	if string(data) != "chunk" {
		t.Fatalf("data = %q", data)
	}
}
