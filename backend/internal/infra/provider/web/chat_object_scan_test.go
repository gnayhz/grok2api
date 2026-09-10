package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type objectChunkReader struct {
	data  []byte
	chunk int
}

type objectReadFunc func([]byte) (int, error)

func (f objectReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestObjectScannerEmitsBeforeNextReadAndPreservesReadError(t *testing.T) {
	reads, frames := 0, 0
	failure := errors.New("upstream reset")
	source := objectReadFunc(func(p []byte) (int, error) {
		reads++
		if reads == 1 {
			return copy(p, `{"token":"first"}`), nil
		}
		if frames != 1 {
			t.Fatal("read ahead before delivering first frame")
		}
		return copy(p, `{"token":"last"}`), failure
	})
	err := consumeJSONObjects(source, 100, func([]byte) error { frames++; return nil })
	if !errors.Is(err, failure) || frames != 2 {
		t.Fatalf("frames=%d err=%v", frames, err)
	}
}

func TestObjectScannerStopsNonProgressingReader(t *testing.T) {
	err := consumeJSONObjects(objectReadFunc(func([]byte) (int, error) { return 0, nil }), 100, func([]byte) error { t.Fatal("unexpected frame"); return nil })
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("err=%v", err)
	}
}

func (r *objectChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data[:min(len(r.data), r.chunk)])
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func TestObjectScannerHandlesEveryChunkBoundary(t *testing.T) {
	objects := []string{`{"a":"escaped \\ quote \" } { 世界","b":[{"n":1}]}`, `{}`, `{"token":"next"}`}
	payload := []byte("prefix\n" + strings.Join(objects, "\n") + "\n")
	for chunk := 1; chunk <= len(payload); chunk++ {
		var got []string
		err := consumeJSONObjects(&objectChunkReader{data: payload, chunk: chunk}, 1<<20, func(data []byte) error { got = append(got, string(data)); return nil })
		if err != nil || strings.Join(got, "\n") != strings.Join(objects, "\n") {
			t.Fatalf("chunk=%d got=%q err=%v", chunk, got, err)
		}
	}
}

func TestObjectScannerBoundsAndErrors(t *testing.T) {
	for _, chunk := range []int{1, 7, 65536} {
		for _, payload := range []string{`{"text":"unfinished`, `{"a":{}`} {
			err := consumeJSONObjects(&objectChunkReader{data: []byte(payload), chunk: chunk}, 1<<20, func([]byte) error { return nil })
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("chunk=%d incomplete=%q err=%v", chunk, payload, err)
			}
		}
		payload := []byte(`{"a":"` + strings.Repeat("x", 80) + `"}`)
		calls := 0
		err := consumeJSONObjects(&objectChunkReader{data: payload, chunk: chunk}, len(payload)-1, func([]byte) error { calls++; return nil })
		if err == nil || calls != 0 {
			t.Fatalf("oversize accepted: calls=%d err=%v", calls, err)
		}
		err = consumeJSONObjects(&objectChunkReader{data: payload, chunk: chunk}, len(payload), func([]byte) error { calls++; return nil })
		if err != nil || calls != 1 {
			t.Fatalf("exact limit rejected: calls=%d err=%v", calls, err)
		}
	}
	want := errors.New("consumer stopped")
	if err := consumeJSONObjects(strings.NewReader(`{}{} `), 100, func([]byte) error { return want }); !errors.Is(err, want) {
		t.Fatalf("callback error=%v", err)
	}
}

func BenchmarkConsumeWebObjects(b *testing.B) {
	for _, size := range []int{80, 8 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			payload := []byte(strings.Repeat(`{"result":{"response":{"token":"`+strings.Repeat("x", size)+`"}}}`+"\n", 256))
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for b.Loop() {
				count := 0
				err := consumeJSONObjects(bytes.NewReader(payload), 1<<20, func([]byte) error { count++; return nil })
				if err != nil || count != 256 {
					b.Fatalf("count=%d err=%v", count, err)
				}
			}
		})
	}
}
