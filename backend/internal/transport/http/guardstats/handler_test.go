package guardstats

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/gin-gonic/gin"
)

// TestGuardStatsEndpointServesSnapshot：端点返回守卫快照的稳定形状——
// 四个固定顺序信号 + 八个豁免原因全部在场（豁免台账是 round 6 起的 UI
// 数据源，缺失会直接把面板砍瞎），且排序稳定保证 UI 轮询行序不跳动。
func TestGuardStatsEndpointServesSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler().Register(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/guard-stats", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Data gateway.GuardStatsSnapshot `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var signals []string
	for _, s := range payload.Data.Signals {
		signals = append(signals, s.Signal)
	}
	wantSignals := []string{"created_timeout", "evidence_timeout", "empty_stream", "missing_thinking"}
	if len(signals) != len(wantSignals) {
		t.Fatalf("signals = %v, want %v", signals, wantSignals)
	}
	for i := range wantSignals {
		if signals[i] != wantSignals[i] {
			t.Fatalf("signal order = %v, want %v", signals, wantSignals)
		}
	}
	var reasons []string
	for _, e := range payload.Data.Exempts {
		reasons = append(reasons, e.Reason)
	}
	if len(reasons) != 8 {
		t.Fatalf("exempt reasons = %v, want the eight canonical tokens", reasons)
	}
	found := map[string]bool{}
	for _, r := range reasons {
		found[r] = true
	}
	for _, want := range []string{"disabled", "skip_input", "operation", "compaction", "provider", "model_out_of_scope", "messages_thinking_off", "model_no_reasoning"} {
		if !found[want] {
			t.Fatalf("exempt reason %q missing from snapshot: %v", want, reasons)
		}
	}
}
