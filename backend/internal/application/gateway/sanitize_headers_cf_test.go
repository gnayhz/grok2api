package gateway

import (
	"net/http"
	"testing"
)

// TestSanitizeHeadersKeepsCFMitigated 锁定 round 54：cf-mitigated（CF 拦截
// 标记）与 cf-cache-status 进入诊断头快照——403 归因的关键证据
// （视频 403 排查曾因白名单缺失丢失现场）。
func TestSanitizeHeadersKeepsCFMitigated(t *testing.T) {
	header := http.Header{}
	header.Set("Cf-Mitigated", "challenge")
	header.Set("Cf-Cache-Status", "DYNAMIC")
	header.Set("Cf-Ray", "8f2a-example")
	header.Set("X-Dangerous", "leak-me")
	snapshot := sanitizeDiagnosticHeaders(header)
	if snapshot["Cf-Mitigated"] == nil || snapshot["Cf-Mitigated"][0] != "challenge" {
		t.Fatalf("cf-mitigated 应保留: %#v", snapshot)
	}
	if snapshot["Cf-Cache-Status"] == nil {
		t.Fatalf("cf-cache-status 应保留")
	}
	if snapshot["Cf-Ray"] == nil {
		t.Fatalf("cf-ray 应保留（既有行为）")
	}
	if _, exists := snapshot["X-Dangerous"]; exists {
		t.Fatalf("非白名单头不应保留")
	}
}
