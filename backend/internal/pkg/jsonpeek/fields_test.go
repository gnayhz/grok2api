package jsonpeek

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestObjectFieldsAndArrayValues(t *testing.T) {
	raw := []byte(`{"nested":{"delta":"wrong"},"\u0064elta":"a\/b\ud83d\ude00","array":[1e2,-0.1,{},["x"],null],"delta":"last"}`)
	var deltas []string
	var count int
	ObjectFields(raw, func(k, v []byte) bool {
		switch string(k) {
		case "delta":
			deltas = append(deltas, string(UnquoteBytes(v)))
		case "array":
			ArrayValues(v, func(value []byte) bool {
				if !json.Valid(value) {
					t.Fatalf("bad value %s", value)
				}
				count++
				return true
			})
		}
		return true
	})
	if len(deltas) != 2 || deltas[0] != "a/b😀" || deltas[1] != "last" || count != 5 {
		t.Fatalf("deltas=%v count=%d", deltas, count)
	}
}

func FuzzValidatedObjectFields(f *testing.F) {
	for _, s := range []string{`{}`, `{"delta":"\ud83d\ude00"}`, `{"x":[{},[1,2],null,true,-1e2],"type":"event"}`, `{"a":"\\\"","b":"ok"}`, `{"type":"reasoning","delta":"ok",`, "{\"x\":\"\x01\"}"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if Valid(data) != json.Valid(data) {
			t.Fatalf("validator disagrees: %q", data)
		}
		if !Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
			return
		}
		var expected map[string]json.RawMessage
		if json.Unmarshal(data, &expected) != nil {
			return
		}
		got := map[string]json.RawMessage{}
		ObjectFields(data, func(k, v []byte) bool { got[string(k)] = append([]byte(nil), v...); return true })
		if len(got) != len(expected) {
			t.Fatalf("fields=%d want=%d: %s", len(got), len(expected), data)
		}
		for k, want := range expected {
			if !bytes.Equal(got[k], want) {
				t.Fatalf("field=%q got=%s want=%s", k, got[k], want)
			}
		}
	})
}

func TestValidatorDepthBoundary(t *testing.T) {
	for _, depth := range []int{4096, 4097, 10000, 10001} {
		data := append(bytes.Repeat([]byte{'['}, depth), bytes.Repeat([]byte{']'}, depth)...)
		if got, want := Valid(data), json.Valid(data); got != want {
			t.Fatalf("depth=%d got=%v want=%v", depth, got, want)
		}
	}
}
