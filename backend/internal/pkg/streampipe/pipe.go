package streampipe

// streampipe 为 SSE/流式转换管道的 io.Pipe 写端 goroutine 提供 panic 隔离。
// 这些 goroutine 直接解析不可信的上游字节流, gin.Recovery 在 Go 语义上无法
// 覆盖 handler 派生的 goroutine——任一转换器 panic 会击穿整个进程。
// 包装器 recover 后以错误关闭 pipe: 客户端得到可重试的流错误(502 语义),
// 进程与其余在途请求不受影响。

import (
	"fmt"
	"io"
	"runtime/debug"
)

// PanicError 表示流管道写端发生 panic;堆栈仅供服务端日志,不返回客户端。
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("流式管道 panic: %v", e.Value)
}

// Run 执行流转换主体 work 并以其结果(或捕获的 panic)关闭 writer。reader
// 由调用方(消费者)持有并关闭——此处绝不触碰 reader:对 io.Pipe 而言提前
// 关闭 reader 会让在途写立即失败, 截断正常转发。仅负责以错误(或正常 EOF)
// 关闭 writer, 消费侧读到错误即结束转发。
func Run(writer *io.PipeWriter, work func() error) {
	var err error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = &PanicError{Value: recovered, Stack: debug.Stack()}
			}
		}()
		err = work()
	}()
	_ = writer.CloseWithError(err)
}
