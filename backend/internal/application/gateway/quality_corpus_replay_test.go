package gateway

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCorpusReplay replays the whole archived trace corpus through the
// CURRENT scanner. Enabled when GROK2API_TRACE_REPLAY_DIR is set;
// skipped otherwise. Oracle covers every archived shape: cipher+zero-delta
// → withhold; summary+completed → deliver; summary+cut → deliver (rule 1
// fired before the cut); text-without-thinking → withhold (rule 3 — applies
// to continuation shapes too, the exemption was revoked; see
// quality_continuation_test.go); zero-evidence/empty → wait+
// errQualityEmptyStream (live manifestation: evidence-timeout hold).
func TestCorpusReplay(t *testing.T) {
	dir := os.Getenv("GROK2API_TRACE_REPLAY_DIR")
	if dir == "" {
		t.Skip("GROK2API_TRACE_REPLAY_DIR not set")
	}
	// 兼容两种目录形态：批次式（batch*/）与去重平铺（unique/）。
	var files []string
	for _, pattern := range []string{filepath.Join(dir, "batch*", "*.sse"), filepath.Join(dir, "*.sse")} {
		matched, err := filepath.Glob(pattern)
		if err == nil {
			files = append(files, matched...)
		}
	}
	if len(files) == 0 {
		t.Fatalf("no traces under %s", dir)
	}
	seen := map[string]bool{}
	var wantW, wantD, gotW, gotD, mismatch, skipped int
	cfg := QualityRetryRuntime{Enabled: true, CreatedTimeout: 30 * time.Second, EvidenceTimeout: 30 * time.Second}
	emptyEnc := `encrypted_content":""`
	completedMark := `"type":"response.completed"`
	for _, f := range files {
		name := filepath.Base(f)
		if seen[name] {
			continue
		}
		seen[name] = true
		raw, err := os.ReadFile(f)
		if err != nil {
			skipped++
			continue
		}
		// Zero-byte captures flow into the zero-evidence branch below and
		// assert the empty-stream hold like any other P0 shape.
		hasSum, enc, done, hasText := false, false, false, false
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			if strings.Contains(line, "encrypted_content") && !strings.Contains(line, emptyEnc) {
				enc = true
			}
			if strings.Contains(line, "reasoning_summary_text.delta") {
				hasSum = true
			}
			if strings.Contains(line, "response.output_text.delta") {
				hasText = true
			}
			if strings.Contains(line, completedMark) {
				done = true
			}
		}
		var want QualityVerdict
		switch {
		case enc && !hasSum:
			want = QualityWithhold
		case hasSum && done:
			want = QualityDeliver
		case hasSum && !done:
			// Guard/upstream cut after thinking evidence: rule 1 must have
			// delivered before the cut.
			want = QualityDeliver
		case hasText && !hasSum:
			// Outrun/plain: text without thinking under the default schedule
			// must withhold (rule 3).
			want = QualityWithhold
		case !enc && !hasSum && !hasText:
			// Zero-evidence stream (P0/empty): EOF manifests as the
			// empty-stream hold — QualityWait plus errQualityEmptyStream.
			want = QualityWait
		default:
			skipped++
			continue
		}
		replay, verdict, _, err := peekQualityStream(t.Context(), io.NopCloser(strings.NewReader(reconstructTeeEventStream(string(raw)))), qualityProtocolResponses, cfg)
		if replay != nil {
			_ = replay.Close()
		}
		if want == QualityWait {
			// Empty-stream hold: verdict Wait with the sentinel error is the
			// expected pair; any other verdict or error is a mismatch.
			if verdict != QualityWait || (err != nil && !errors.Is(err, errQualityEmptyStream)) {
				mismatch++
				t.Errorf("%s: verdict=%s err=%v (want wait/empty-stream)", name, verdict, err)
			} else {
				wantW++
				gotW++
			}
			continue
		}
		if err != nil {
			mismatch++
			t.Errorf("%s: peek err %v (want %s)", name, err, want)
			continue
		}
		if want == QualityWithhold {
			wantW++
		} else {
			wantD++
		}
		if verdict == want {
			if verdict == QualityWithhold {
				gotW++
			} else {
				gotD++
			}
		} else {
			mismatch++
			t.Errorf("%s: verdict=%s want=%s", name, verdict, want)
		}
	}
	t.Logf("corpus replay: withhold %d/%d, deliver %d/%d, skipped %d, mismatch %d", gotW, wantW, gotD, wantD, skipped, mismatch)
	if mismatch > 0 {
		t.Fatalf("%d mismatches", mismatch)
	}
}

// reconstructTeeEventStream 还原 tee 轨迹文件中被切割的事件。tee 以 chunk 落盘
// 并注入 #ts 毫秒标记：巨型密文行（D-a 降智签名，数十 KB 单行）会被标记与
// 换行打碎；线上原始流是单行事件（live 扫描器规则 2 定罪正确），回放直接喂
// 文件会令载荷解析失败、规则 2 信号丢失（规模轮 196 首次失配实证）。重建规
// 则：不以 data:/event:/#ts/注释/空行开头的行是前一个 data 载荷的续段，直
// 接拼接（JSON 载荷不含裸换行，拼接无损）。
func reconstructTeeEventStream(raw string) string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			for i+1 < len(lines) {
				next := lines[i+1]
				if next == "" || strings.HasPrefix(next, "data:") || strings.HasPrefix(next, "event:") || strings.HasPrefix(next, "#ts ") || strings.HasPrefix(next, ": ") {
					break
				}
				payload += next
				i++
			}
			out = append(out, "data: "+payload)
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
