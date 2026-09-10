package responsebuffer

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
)

func TestBudgetGrowthCountsBothArraysAndReleasesBorrow(t *testing.T) {
	pool := NewPool(16 << 10)
	budget := pool.Request(12 << 10)
	buffer := New(budget, 12<<10)
	if _, err := buffer.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if got := budget.Snapshot(); got.Used != 8192 || got.Peak != 12288 {
		t.Fatalf("growth accounting: %+v", got)
	}
	if _, err := buffer.Write(make([]byte, 4096)); !errors.Is(err, ErrExhausted) {
		t.Fatalf("growth did not reject before allocating: %v", err)
	}
	body := buffer.Body()
	data, release, ok := body.BorrowBytes()
	if !ok || len(data) != 4097 {
		t.Fatal("borrow lost bytes")
	}
	_ = body.Close()
	if got := budget.Snapshot().Used; got != 8192 {
		t.Fatalf("closed active borrow released %d", got)
	}
	if data[4096] != 1 {
		t.Fatal("borrow changed on close")
	}
	release()
	release()
	if got := pool.Snapshot(); got.Used != 0 || got.Rejected != 1 {
		t.Fatalf("reservation leak: %+v", got)
	}
}

func TestSharedProcessBudgetUnderConcurrentRequests(t *testing.T) {
	pool := NewPool(64 << 10)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				buffer := New(pool.Request(16<<10), 16<<10)
				_, err := buffer.Write(make([]byte, 8192))
				if err != nil && !errors.Is(err, ErrExhausted) {
					t.Error(err)
				}
				body := buffer.Body()
				_, release, ok := body.BorrowBytes()
				_ = body.Close()
				if ok {
					release()
				}
			}
		}()
	}
	wg.Wait()
	if got := pool.Snapshot(); got.Used != 0 || got.Peak > got.Limit {
		t.Fatalf("process budget violated: %+v", got)
	}
}

func TestReadAllLimitAndReadError(t *testing.T) {
	for _, size := range []int{0, 4095, 4096, 4097, 32000} {
		pool := NewPool(1 << 20)
		body, err := ReadAll(bytes.NewReader(make([]byte, size)), pool.Request(1<<20), 4096)
		if (size > 4096) != errors.Is(err, ErrLimit) {
			t.Fatalf("size=%d: %v", size, err)
		}
		data, release, ok := body.BorrowBytes()
		if !ok {
			t.Fatal("missing buffered body")
		}
		// A borrowed slice keeps its reservation until explicitly released.
		release()
		_ = data
		_ = body.Close()
		if pool.Snapshot().Used != 0 {
			t.Fatal("limit path retained memory")
		}
	}
	pool := NewPool(1 << 20)
	body, err := ReadAll(&lastReadError{}, pool.Request(1<<20), 4096)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	data, release, _ := body.BorrowBytes()
	if string(data) != "partial" {
		t.Fatalf("lost partial read: %q", data)
	}
	release()
	_ = body.Close()
	if pool.Snapshot().Used != 0 {
		t.Fatal("error path retained memory")
	}
}

type lastReadError struct{}

func (*lastReadError) Read(p []byte) (int, error) { return copy(p, "partial"), io.ErrUnexpectedEOF }
