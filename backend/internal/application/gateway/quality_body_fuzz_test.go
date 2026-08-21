package gateway

import (
	"io"
	"testing"
)

// FuzzPeekQualityBody feeds arbitrary non-streaming bodies into the body
// classifier. Invariants: no panic on any input; the replayed body is
// byte-identical to the input (the guard must never mutate or drop payload
// on the non-streaming path); counters stay non-negative on every verdict.
// Covers the shape-analysis surface that FuzzObserveQualityChunk (streaming)
// does not: aggregate output items, summary/content reasoning text, refusal
// fallbacks, and unknown-shape fail-open.
func FuzzPeekQualityBody(f *testing.F) {
	f.Add([]byte(`{"id":"r1","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"output_tokens":9}}`))
	f.Add([]byte(`{"output":[]}`))
	f.Add([]byte(`{"choices":[{"message":{"content":"alien shape"}}]}`))
	f.Add([]byte("not-json"))
	f.Add([]byte(`{"output":[{"type":"function_call","call_id":"c1","name":"read"}]}`))
	f.Add([]byte(`{"output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]},{"type":"web_search_call"}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		cfg := QualityRetryRuntime{Enabled: true, MinOutputTokens: 8}
		replay, verdict, usage, _, err := peekQualityBody(io.NopCloser(newByteReader(body)), cfg)
		if replay == nil {
			t.Fatalf("replay reader must never be nil")
		}
		replayed, readErr := io.ReadAll(replay)
		if readErr != nil {
			t.Fatalf("replay read failed: %v", readErr)
		}
		if string(replayed) != string(body) {
			t.Fatalf("replay mutated payload: in=%d bytes out=%d bytes", len(body), len(replayed))
		}
		if usage.OutputTokens < 0 || usage.ReasoningTokens < 0 || usage.InputTokens < 0 {
			t.Fatalf("negative usage counters: %#v", usage)
		}
		if err == nil && verdict == QualityWait {
			t.Fatalf("nil error with Wait verdict is inconsistent: %q", body)
		}
	})
}

// newByteReader avoids importing bytes solely for the fuzz harness.
func newByteReader(b []byte) io.Reader {
	return &byteSliceReader{b: b}
}

type byteSliceReader struct {
	b []byte
	i int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
