package conversation

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"
)

var sizeEquivCipherBytes = []int{4 << 10, 63 << 10, 65 << 10, 2 << 20}

func sizeEquivUpstream(cipher string) string {
	return strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6"}}`,
		"",
		"event: response.reasoning_text.delta",
		`data: {"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"plan"}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		"",
		"event: response.output_item.done",
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}}",
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"answer"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":12,"output_tokens":95,"output_tokens_details":{"reasoning_tokens":40}}}}`,
		"",
		"",
	}, "\n")
}

func convertSizeEquiv(t *testing.T, cipher string, op string, opts ResponseOptions) []byte {
	t.Helper()
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(sizeEquivUpstream(cipher))), op, opts))
	if err != nil {
		t.Fatal(err)
	}
	// 归一化墙钟字段：转换器的 created 取 time.Now().Unix()，基准与后续
	// 转换跨秒边界时该字段差 1（同长度不同内容）——race 全量
	// 门实测翻牌根因。等价性比的是密文尺寸是否影响输出，不是时间戳。
	return createdRe.ReplaceAll(converted, []byte(`"created":0`))
}

var createdRe = regexp.MustCompile(`"created":[0-9]+`)

func TestChatConversionSizeEquivalence(t *testing.T) {
	t.Parallel()
	baseline := convertSizeEquiv(t, strings.Repeat("B", sizeEquivCipherBytes[0]), OperationChat, ResponseOptions{})
	if bytes.Contains(baseline, []byte("encrypted_content")) {
		t.Fatal("chat leaked ciphertext")
	}
	if !bytes.Contains(baseline, []byte(`"reasoning_content":"plan"`)) || !bytes.Contains(baseline, []byte(`"content":"answer"`)) {
		t.Fatalf("baseline missing deltas: %s", baseline)
	}
	for _, size := range sizeEquivCipherBytes[1:] {
		got := convertSizeEquiv(t, strings.Repeat("B", size), OperationChat, ResponseOptions{})
		if !bytes.Equal(got, baseline) {
			t.Fatalf("chat conversion changed at cipher %d: got %d bytes want %d", size, len(got), len(baseline))
		}
	}
}

func TestAnthropicSignatureSizeEquivalence(t *testing.T) {
	t.Parallel()
	opts := ResponseOptions{AnthropicThinking: true}
	for _, size := range sizeEquivCipherBytes {
		cipher := strings.Repeat("C", size)
		got := convertSizeEquiv(t, cipher, OperationMessages, opts)
		if bytes.Contains(got, []byte("encrypted_content")) {
			t.Fatal("anthropic leaked encrypted_content key")
		}
		sig := anthropicSignature(t, got)
		if sig != cipher {
			t.Fatalf("cipher %d: signature len %d want %d", size, len(sig), len(cipher))
		}
		if !bytes.Contains(got, []byte(`"thinking":"plan"`)) || !bytes.Contains(got, []byte(`"text":"answer"`)) {
			t.Fatalf("cipher %d missing deltas", size)
		}
	}
}

func anthropicSignature(t *testing.T, stream []byte) string {
	t.Helper()
	for _, line := range bytes.Split(stream, []byte{10}) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		if event.Delta.Type == "signature_delta" {
			return event.Delta.Signature
		}
	}
	t.Fatal("no signature_delta")
	return ""
}

func FuzzConsumeSSE(f *testing.F) {
	f.Add([]byte("event: x\ndata: {}\n\n"))
	f.Add([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	f.Add([]byte(": comment\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			data = data[:1<<20]
		}
		_ = consumeSSE(bytes.NewReader(data), func(string, []byte) error { return nil })
	})
}
