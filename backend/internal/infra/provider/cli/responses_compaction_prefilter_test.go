package cli

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestExpandGatewayCompactionHistoryPrefilter 锁定预筛契约:
// body 不含 "compaction" 字面量时原样返回(零展开、零计数),不做全量
// 装箱解码。含字面量的 body(含歧义命中,如用户消息里的普通文本)仍走
// 全量解码路径,行为与预筛前一致。
func TestExpandGatewayCompactionHistoryPrefilter(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	codec := newGatewayCompactionCodec(cipher)

	// 无字面量:大 body 原样返回。
	bigBody := []byte(`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"` +
		strings.Repeat("ordinary conversation text ", 4000) + `"}],"instructions":"be helpful"}`)
	expanded, foreign, drifted, err := expandGatewayCompactionHistory(bigBody, codec, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if &bigBody[0] != &expanded[0] || foreign != 0 || drifted != 0 {
		t.Fatal("no-literal body must return unchanged without expansion")
	}

	// 歧义命中(字面量出现在用户文本里,非类型字段):走全量解码,
	// 没有 type==compaction 的项,同样原样返回(字节相同)。
	ambiguous := []byte(`{"input":[{"type":"message","role":"user","content":"the word 'compaction' appeared in prose"}]}`)
	expanded, foreign, drifted, err = expandGatewayCompactionHistory(ambiguous, codec, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(expanded) != string(ambiguous) || foreign != 0 || drifted != 0 {
		t.Fatalf("ambiguous body changed: %s foreign=%d drifted=%d", expanded, foreign, drifted)
	}
}

// BenchmarkExpandGatewayCompactionHistoryNoCompaction 量化预筛收益:
// 128KB 无压缩项 body 的展开开销(每个 Responses 请求都会执行)。
func BenchmarkExpandGatewayCompactionHistoryNoCompaction(b *testing.B) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		b.Fatal(err)
	}
	codec := newGatewayCompactionCodec(cipher)
	body := []byte(`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"` +
		strings.Repeat("ordinary conversation text ", 4000) + `"}],"instructions":"be helpful"}`)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := expandGatewayCompactionHistory(body, codec, "session-1"); err != nil {
			b.Fatal(err)
		}
	}
}
