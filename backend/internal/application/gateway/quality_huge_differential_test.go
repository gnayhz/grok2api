package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestHugeLineCiphertextDifferential：同一密文降智签名以普通尺寸与超大尺寸
// （>64KiB 走 jsonpeek 首尾窗路径）两种行宽到达时必须同判 Withhold——
// 超长行路径的类型识别（head 窗 type/rs_ 探测）与全量解析路径的规则 2
// 是同一判决的两种实现，差分锁定其一致性。
func TestHugeLineCiphertextDifferential(t *testing.T) {
	t.Parallel()
	nl := "\n\n"
	mkStream := func(cipherKiB int) string {
		ct := strings.Repeat("A", cipherKiB*1024)
		return "data: " + `{"type":"response.created","response":{"id":"r1"}}` + nl + "data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"` + ct + `"}}` + nl
	}
	for _, size := range []struct {
		name string
		kib  int
	}{
		{name: "small line full-parse", kib: 8},
		{name: "huge line head-tail window", kib: 256},
	} {
		t.Run(size.name, func(t *testing.T) {
			t.Parallel()
			replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(mkStream(size.kib))), qualityProtocolResponses, QualityRetryRuntime{})
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if replay != nil {
				_ = replay.Close()
			}
			if verdict != QualityWithhold {
				t.Fatalf("verdict = %s, want withhold at any line width", verdict)
			}
		})
	}
}
