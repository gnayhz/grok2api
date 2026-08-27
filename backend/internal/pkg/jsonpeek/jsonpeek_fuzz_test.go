package jsonpeek

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

func FuzzStringField(f *testing.F) {
	f.Add([]byte(`{"type":"response.completed","id":"resp_1"}`), "type")
	f.Add([]byte(`{ "delta" : "hi" }`), "delta")
	f.Add([]byte(`{"encrypted_content":"AAA","type":"x"}`), "type")
	f.Add([]byte(`{"delta":""}`), "delta")
	f.Fuzz(func(t *testing.T, data []byte, key string) {
		got := StringField(data, key)
		if key == "" || !json.Valid(data) {
			return
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil {
			return
		}
		raw, ok := obj[key]
		if !ok {
			return
		}
		var want string
		if json.Unmarshal(raw, &want) != nil {
			return
		}
		if bytes.IndexByte(raw, '\\') >= 0 || !utf8.Valid(raw) {
			return
		}
		needle := make([]byte, 0, len(key)+2)
		needle = append(needle, '"')
		needle = append(needle, key...)
		needle = append(needle, '"')
		if bytes.Count(data, needle) != 1 {
			return
		}
		if got != want {
			t.Fatalf("StringField(%q) = %q want %q in %s", key, got, want, data)
		}
	})
}

func FuzzIntField(f *testing.F) {
	f.Add([]byte(`{"output_tokens":95}`), "output_tokens")
	f.Add([]byte(`{"output_tokens":-3}`), "output_tokens")
	f.Add([]byte(`{"output_tokens":"nope"}`), "output_tokens")
	f.Fuzz(func(t *testing.T, data []byte, key string) {
		got, ok := IntField(data, key)
		if key == "" || !json.Valid(data) {
			return
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil {
			return
		}
		raw, exists := obj[key]
		if !exists {
			return
		}
		var want int64
		if json.Unmarshal(raw, &want) != nil {
			return
		}
		needle := make([]byte, 0, len(key)+2)
		needle = append(needle, '"')
		needle = append(needle, key...)
		needle = append(needle, '"')
		if bytes.Count(data, needle) != 1 {
			return
		}
		if !ok || got != want {
			t.Fatalf("IntField(%q) = %d ok=%v want %d in %s", key, got, ok, want, data)
		}
	})
}

func FuzzRawValue(f *testing.F) {
	f.Add([]byte(`{"error":{"code":"server_error","message":"boom"}}`), "error")
	f.Add([]byte(`{"encrypted_content":"AAA"}`), "encrypted_content")
	f.Add([]byte(`{"n":null}`), "n")
	f.Fuzz(func(t *testing.T, data []byte, key string) {
		raw := RawValue(data, key)
		if key == "" || !json.Valid(data) {
			return
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil {
			return
		}
		want, ok := obj[key]
		if !ok {
			return
		}
		needle := make([]byte, 0, len(key)+2)
		needle = append(needle, '"')
		needle = append(needle, key...)
		needle = append(needle, '"')
		if bytes.Count(data, needle) != 1 {
			return
		}
		if !json.Valid(raw) {
			t.Fatalf("RawValue(%q) invalid %s", key, raw)
		}
		var got any
		var wantVal any
		if json.Unmarshal(raw, &got) != nil || json.Unmarshal(want, &wantVal) != nil {
			return
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(wantVal)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("RawValue(%q) = %s want %s", key, gotJSON, wantJSON)
		}
	})
}

func FuzzTokenUsageFrom(f *testing.F) {
	f.Add([]byte(`{"usage":{"output_tokens":95,"output_tokens_details":{"reasoning_tokens":40}}}`))
	f.Add([]byte(`{"prompt_tokens":1,"completion_tokens":2}`))
	f.Add([]byte(`{"Completion_tokens":1}`))
	f.Add([]byte(`{"outputTokens":8}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		usage := TokenUsageFrom(data)
		if !json.Valid(data) {
			return
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil {
			return
		}
		src := obj["usage"]
		if len(src) == 0 {
			src = data
		}
		// encoding/json struct tags are case-insensitive; jsonpeek is not.
		// Only compare when the exact production keys are present.
		hasOutput := bytes.Contains(src, []byte(`"output_tokens"`))
		hasCompletion := bytes.Contains(src, []byte(`"completion_tokens"`))
		hasOutputCamel := bytes.Contains(src, []byte(`"outputTokens"`))
		if !hasOutput && !hasCompletion && !hasOutputCamel {
			return
		}
		var parsed struct {
			OutputTokens      int64 `json:"output_tokens"`
			CompletionTokens  int64 `json:"completion_tokens"`
			OutputTokensCamel int64 `json:"outputTokens"`
		}
		if json.Unmarshal(src, &parsed) != nil {
			return
		}
		var want int64
		switch {
		case hasOutput:
			want = parsed.OutputTokens
		case hasCompletion:
			want = parsed.CompletionTokens
		case hasOutputCamel:
			want = parsed.OutputTokensCamel
		}
		if want != 0 && (!usage.Found || usage.Output != want) {
			t.Fatalf("TokenUsageFrom output=%d found=%v want %d", usage.Output, usage.Found, want)
		}
	})
}
