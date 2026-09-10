package responseflow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

func TestHoldAndEncodingShareOneAssemblyAcrossPartitions(t *testing.T) {
	raw := "\xef\xbb\xbf: keep\r\nevent: response.delta\r\nid: 7\r\ndata: {\"type\":\"response.reasoning_text.delta\",\r\ndata: \"delta\":\"plan\"}\r\n\r\ndata: [DONE]\n\n"
	for _, partition := range []int{1, 2, 7, 32 << 10} {
		pool := responsebuffer.NewPool(1 << 20)
		stream := New(&fragmentedReader{Reader: strings.NewReader(raw), chunk: partition}, pool.Request(1<<20))
		observed := 0
		var heldData *byte
		stream.Observe(func(event *Event) { observed++ })
		err := stream.Hold(4<<20, func(event *Event) (bool, error) {
			if string(event.Data) != "{\"type\":\"response.reasoning_text.delta\",\n\"delta\":\"plan\"}" {
				t.Fatalf("partition=%d data=%q", partition, event.Data)
			}
			heldData = &event.Data[0]
			return true, nil
		})
		if err != nil || observed != 1 {
			t.Fatalf("hold err=%v observed=%d", err, observed)
		}
		var replay bytes.Buffer
		encoded := 0
		err = stream.Consume(func(event *Event) error {
			if encoded == 0 && heldData != &event.Data[0] {
				t.Fatal("held event was reassembled/copied")
			}
			encoded++
			_, err := replay.Write(event.Raw)
			return err
		})
		_ = stream.Close()
		if err != nil || replay.String() != raw || encoded != 2 || observed != 2 {
			t.Fatalf("err=%v observed=%d encoded=%d replay=%q", err, observed, encoded, replay.String())
		}
		if pool.Snapshot().Used != 0 {
			t.Fatal("stream leaked its event/read allocations")
		}
	}
}

func TestCloseInterruptsBlockedAssemblyAndReleasesHold(t *testing.T) {
	pool := responsebuffer.NewPool(1 << 20)
	reader, writer := io.Pipe()
	stream := New(reader, pool.Request(1<<20))
	done := make(chan error, 1)
	go func() { done <- stream.Hold(4<<20, func(*Event) (bool, error) { return false, nil }) }()
	_, _ = writer.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
	_ = stream.Close()
	_ = writer.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed hold succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("close left the raw reader running")
	}
	if pool.Snapshot().Used != 0 {
		t.Fatal("canceled stream retained buffers")
	}
}

func TestLimitsBeforeAssemblyAndObservers(t *testing.T) {
	for _, processLimit := range []int64{1, 1 << 20} {
		pool := responsebuffer.NewPool(processLimit)
		stream := New(io.NopCloser(strings.NewReader("data: "+strings.Repeat("x", 4096)+"\n\n")), pool.Request(1<<20))
		var observed atomic.Int64
		stream.Observe(func(*Event) { observed.Add(1) })
		err := stream.Hold(1024, func(*Event) (bool, error) { return true, nil })
		_ = stream.Close()
		want := responsebuffer.ErrLimit
		if processLimit == 1 {
			want = responsebuffer.ErrExhausted
		}
		if !errors.Is(err, want) || observed.Load() != 0 {
			t.Fatalf("limit=%d err=%v observations=%d", processLimit, err, observed.Load())
		}
		if got := pool.Snapshot(); got.Used != 0 || got.Peak > got.Limit {
			t.Fatalf("limit accounting %+v", got)
		}
	}
}

func TestRawReadPreservesErrorAfterLastBytes(t *testing.T) {
	source := &fragmentedReader{Reader: io.MultiReader(strings.NewReader("data: {}\n\n"), failingReader{}), chunk: 2}
	stream := New(source, nil)
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if string(data) != "data: {}\n\n" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

type fragmentedReader struct {
	io.Reader
	chunk int
}

func (r *fragmentedReader) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:min(len(p), r.chunk)])
}
func (*fragmentedReader) Close() error { return nil }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestCloseDoesNotWaitForBlockedEncoder(t *testing.T) {
	pool := responsebuffer.NewPool(1 << 20)
	stream := New(io.NopCloser(strings.NewReader("data: {}\n\n")), pool.Request(1<<20))
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- stream.Consume(func(event *Event) error {
			close(started)
			<-release
			if string(event.Data) != "{}" {
				t.Error("close invalidated a borrowed event")
			}
			return nil
		})
	}()
	<-started
	closed := make(chan struct{})
	go func() { _ = stream.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream close waited for downstream writer")
	}
	if pool.Snapshot().Used == 0 {
		t.Fatal("active encoder's event was released prematurely")
	}
	close(release)
	<-done
	if pool.Snapshot().Used != 0 {
		t.Fatal("encoder retained an event after returning")
	}
}

func BenchmarkSharedAdmissionAndEncoding(b *testing.B) {
	raw := []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"plan\"}\n\n" + strings.Repeat("data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n", 1000))
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		stream := New(io.NopCloser(bytes.NewReader(raw)), nil)
		_ = stream.Hold(4<<20, func(*Event) (bool, error) { return true, nil })
		_ = stream.Consume(func(*Event) error { return nil })
		_ = stream.Close()
	}
}

func TestBoundedConsumptionLimitsAssemblyAndCountsAdmissionReplay(t *testing.T) {
	prefix := "data: {\"delta\":\"thinking\"}\n\n"
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprint(overflow), func(t *testing.T) {
			raw := prefix
			if overflow {
				raw += "data: " + strings.Repeat("x", 2<<20) + "\n\n"
			}
			reader := &countingReadCloser{Reader: strings.NewReader(raw)}
			pool := responsebuffer.NewPool(2 << 20)
			stream := New(reader, pool.Request(2<<20))
			if err := stream.Hold(1<<20, func(*Event) (bool, error) { return true, nil }); err != nil {
				t.Fatal(err)
			}
			calls := 0
			err := stream.ConsumeLimit(len(prefix), func(*Event) error { calls++; return nil })
			if overflow != errors.Is(err, responsebuffer.ErrLimit) || (!overflow && err != nil) || calls != 1 {
				t.Fatalf("overflow=%v err=%v callbacks=%d", overflow, err, calls)
			}
			if reader.bytes > readBufferBytes+len(prefix) {
				t.Fatalf("assembled oversized event beyond remaining budget: %d", reader.bytes)
			}
			stream.Close()
			if pool.Snapshot().Used != 0 {
				t.Fatal("retained budget after close")
			}
		})
	}
}

type countingReadCloser struct {
	io.Reader
	bytes int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}
func (*countingReadCloser) Close() error { return nil }
