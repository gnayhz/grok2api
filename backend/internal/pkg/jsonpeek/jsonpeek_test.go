package jsonpeek

import "testing"

func TestStringField(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"hmm"}`)
	if got := StringField(payload, "type"); got != "response.reasoning_text.delta" {
		t.Fatalf("type = %q", got)
	}
	if got := StringField(payload, "item_id"); got != "rs_1" {
		t.Fatalf("item_id = %q", got)
	}
	spaced := []byte(`{ "type" : "response.completed" }`)
	if got := StringField(spaced, "type"); got != "response.completed" {
		t.Fatalf("spaced type = %q", got)
	}
	if got := StringField([]byte(`{"delta":""}`), "delta"); got != "" {
		t.Fatalf("empty delta = %q", got)
	}
	cipher := []byte(`{"type":"x","encrypted_content":"AAA"}`)
	if got := StringField(cipher, "type"); got != "x" {
		t.Fatalf("type before ciphertext = %q", got)
	}
}

func TestIntFieldAndTokenUsageFrom(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"id":"resp_1","output":[{"encrypted_content":"AAA"}],"usage":{"output_tokens":5000,"output_tokens_details":{"reasoning_tokens":4000},"input_tokens":12,"total_tokens":5012,"cache_creation_input_tokens":7,"cost_in_usd_ticks":99,"num_sources_used":3,"num_server_side_tools_used":1,"context_details":{"input_tokens":10,"output_tokens":2}}}`)
	if v, ok := IntField(payload, "output_tokens"); !ok || v != 5000 {
		t.Fatalf("output_tokens = %d ok=%v", v, ok)
	}
	usage := TokenUsageFrom(payload)
	if !usage.Found || usage.Output != 5000 || usage.Reasoning != 4000 || usage.Input != 12 || usage.Total != 5012 || usage.CacheCreation != 7 || usage.CostTicks != 99 || usage.Sources != 3 || usage.ServerTools != 1 || usage.ContextInput != 10 || usage.ContextOutput != 2 {
		t.Fatalf("usage = %+v", usage)
	}
	if _, ok := IntField([]byte(`{"output_tokens":"nope"}`), "output_tokens"); ok {
		t.Fatal("string value must not parse as int")
	}
	camel := TokenUsageFrom([]byte(`{"usage":{"inputTokens":3,"outputTokens":4,"totalTokens":7}}`))
	if !camel.Found || camel.Input != 3 || camel.Output != 4 || camel.Total != 7 {
		t.Fatalf("camelCase usage = %+v", camel)
	}
	wrongCase := TokenUsageFrom([]byte(`{"Completion_tokens":1}`))
	if wrongCase.Found || wrongCase.Output != 0 {
		t.Fatalf("wrong-case key must not match: %+v", wrongCase)
	}
}

func TestRootStringFieldIgnoresNestedType(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"response":{"output":[{"type":"reasoning"}]},"type":"response.failed"}`)
	if got := StringField(payload, "type"); got != "reasoning" {
		t.Fatalf("StringField type = %q", got)
	}
	if got := RootStringField(payload, "type"); got != "response.failed" {
		t.Fatalf("RootStringField type = %q", got)
	}
}

func TestRootStringFieldScanOrderIndependent(t *testing.T) {
	t.Parallel()
	// 兼容层经 map[string]any 重排后的键序：response 在 type 之前。
	sorted := []byte(`{"response":{"id":"resp_1","usage":{"output_tokens":7}},"sequence_number":9,"type":"response.completed"}`)
	if got := RootStringFieldScan(sorted, "type"); got != "response.completed" {
		t.Fatalf("sorted type = %q", got)
	}
	if got := RootStringFieldScan(sorted, "id"); got != "" {
		t.Fatalf("root id must stay empty (nested only): %q", got)
	}
	huge := []byte(`{"response":{"output":[{"encrypted_content":"` + string(make([]byte, 1<<20)) + `"}]},"type":"response.failed"}`)
	if got := RootStringFieldScan(huge, "type"); got != "response.failed" {
		t.Fatalf("huge sorted type = %q", got)
	}
	// 截断缓冲：目标键在被截断的值之后必须返回空串。
	truncated := sorted[:30]
	if got := RootStringFieldScan(truncated, "type"); got != "" {
		t.Fatalf("truncated type = %q", got)
	}
	// 嵌套 type 不得误命中（与 RootStringField 同口径）。
	nested := []byte(`{"item":{"type":"message"},"type":"response.completed"}`)
	if got := RootStringFieldScan(nested, "type"); got != "response.completed" {
		t.Fatalf("nested-first type = %q", got)
	}
	if got := RootStringFieldScan([]byte(`[1,2]`), "type"); got != "" {
		t.Fatalf("non-object = %q", got)
	}
	if got := RootStringFieldScan(nil, "type"); got != "" {
		t.Fatalf("nil = %q", got)
	}
}

func TestRootIntFieldScanOrderIndependent(t *testing.T) {
	t.Parallel()
	// 重排键序大帧：sequence_number 位于多 KB response 对象之后。
	sorted := []byte("{\"response\":{\"output\":[{\"text\":\"" + string(make([]byte, 8192)) + "\"}]},\"sequence_number\":41,\"type\":\"response.completed\"}")
	if got, ok := RootIntFieldScan(sorted, "sequence_number"); !ok || got != 41 {
		t.Fatalf("sorted sequence_number = %d ok=%v", got, ok)
	}
	plain := []byte("{\"sequence_number\":7,\"type\":\"x\"}")
	if got, ok := RootIntFieldScan(plain, "sequence_number"); !ok || got != 7 {
		t.Fatalf("plain sequence_number = %d ok=%v", got, ok)
	}
	if got, ok := RootIntFieldScan([]byte("{\"n\":-3}"), "n"); !ok || got != -3 {
		t.Fatalf("negative = %d ok=%v", got, ok)
	}
	if _, ok := RootIntFieldScan([]byte("{\"n\":\"7\"}"), "n"); ok {
		t.Fatal("string value must not parse as int")
	}
	if _, ok := RootIntFieldScan([]byte("{\"n\":true}"), "n"); ok {
		t.Fatal("bool value must not parse as int")
	}
	if _, ok := RootIntFieldScan([]byte("{\"item\":{\"sequence_number\":9}}"), "sequence_number"); ok {
		t.Fatal("nested value must not match")
	}
	if _, ok := RootIntFieldScan(sorted[:60], "sequence_number"); ok {
		t.Fatal("truncated buffer must not match")
	}
}

func TestRawValueExtractsErrorFromTruncatedDocument(t *testing.T) {
	t.Parallel()
	prefix := []byte(`{"type":"response.failed","response":{"id":"resp_1","error":{"code":"server_error","message":"boom"},"output":[{"encrypted_content":"AAAA`)
	raw := RawValue(prefix, "error")
	if string(raw) != `{"code":"server_error","message":"boom"}` {
		t.Fatalf("raw error = %s", raw)
	}
	if RawValue(prefix[:20], "error") != nil {
		t.Fatal("incomplete error object must not parse")
	}
}
