package responseflow

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

func FuzzCanonicalEventPartitions(f *testing.F) {
	f.Add([]byte("\xef\xbb\xbfevent: response.delta\r\ndata: {\"type\":\"response.reasoning_text.delta\",\r\ndata: \"delta\":\"plan\"}\r\n\r\ndata: [DONE]\n\n"), uint16(1))
	f.Add([]byte(": comment\n\ndata: {}\n\ndata: {\"unfinished\":"), uint16(7))
	f.Add([]byte("data: {\"delta\":\"line\\n\\u4e2d\"}\r\n\r\n"), uint16(13))
	f.Fuzz(func(t *testing.T, raw []byte, partition uint16) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		type eventValue struct {
			Raw, Data, Kind string
			HasData         bool
		}
		read := func(chunk int) ([]eventValue, string) {
			pool := responsebuffer.NewPool(64 << 20)
			stream := New(&fragmentedReader{Reader: bytes.NewReader(raw), chunk: chunk}, pool.Request(64<<20))
			var values []eventValue
			err := stream.Consume(func(event *Event) error {
				values = append(values, eventValue{string(event.Raw), string(event.Data), string(event.Kind), event.HasData})
				return nil
			})
			_ = stream.Close()
			if pool.Snapshot().Used != 0 {
				t.Fatal("partitioned parser retained resources")
			}
			if err != nil && err != io.EOF {
				return values, err.Error()
			}
			return values, ""
		}
		whole, wholeErr := read(len(raw) + 1)
		parts, partsErr := read(1 + int(partition)%4096)
		if wholeErr != partsErr || !reflect.DeepEqual(whole, parts) {
			t.Fatalf("chunk partition changed canonical events: whole=%+v/%q parts=%+v/%q", whole, wholeErr, parts, partsErr)
		}
	})
}
