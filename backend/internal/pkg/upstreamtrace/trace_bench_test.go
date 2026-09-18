package upstreamtrace

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptrace"
	"testing"
)

type traceChunkReader struct{ *bytes.Reader }

func (r traceChunkReader) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:min(len(p), 512)])
}

func BenchmarkStreamTracing(b *testing.B) {
	for _, size := range []int{4 << 10, 1 << 20} {
		for _, mode := range []string{"disabled", "network", "raw"} {
			b.Run(fmt.Sprintf("bytes_%d/%s", size, mode), func(b *testing.B) {
				directory := b.TempDir()
				data := bytes.Repeat([]byte("x"), size)
				buffer := make([]byte, 32<<10)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				for range b.N {
					if mode != "disabled" {
						ctx, finish := network(context.Background(), "build", "responses", directory)
						hooks := httptrace.ContextClientTrace(ctx)
						hooks.GetConn("synthetic")
						hooks.GotConn(httptrace.GotConnInfo{Reused: true})
						hooks.WroteRequest(httptrace.WroteRequestInfo{})
						hooks.GotFirstResponseByte()
						finish()
					}
					// Hide bytes.Reader.WriteTo so all modes consume the same chunks.
					var source io.ReadCloser = io.NopCloser(struct{ io.Reader }{traceChunkReader{bytes.NewReader(data)}})
					if mode == "raw" {
						DumpRequest(directory, "responses", "synthetic", true, data)
						source = TeeStream(directory, "responses", "synthetic", source)
					}
					_, err := io.CopyBuffer(io.Discard, source, buffer)
					closeErr := source.Close()
					if err != nil || closeErr != nil {
						b.Fatalf("copy=%v close=%v", err, closeErr)
					}
				}
				b.StopTimer()
			})
		}
	}
}
