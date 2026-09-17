package account

import (
	"encoding/json"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
)

// TestAccountImportResponseMapsEveryCounter 锁定导入与 Web→Console 同步
// 共用完成事件的全部计数字段。此前同步路径漏映射 Failed，前端把部分失败
// 的同步批次读成全部成功；导出面同时是前端契约，缺字段即静默错误。
func TestAccountImportResponseMapsEveryCounter(t *testing.T) {
	result := accountapp.ImportResult{Created: 3, Updated: 5, Skipped: 7, Failed: 11}
	syncResult := accountsyncapp.Result{Succeeded: 13, Failed: 17}

	payload, err := json.Marshal(newAccountImportResponse(result, syncResult))
	if err != nil {
		t.Fatalf("marshal completion event: %v", err)
	}
	var decoded map[string]int
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal completion event: %v", err)
	}
	want := map[string]int{"created": 3, "updated": 5, "skipped": 7, "failed": 11, "synced": 13, "syncFailed": 17}
	for field, expected := range want {
		actual, present := decoded[field]
		if !present {
			t.Errorf("completion event is missing %q; every counter is part of the client contract", field)
			continue
		}
		if actual != expected {
			t.Errorf("completion event %q = %d, want %d", field, actual, expected)
		}
	}
	if len(decoded) != len(want) {
		t.Errorf("completion event has %d fields, want %d: %v", len(decoded), len(want), decoded)
	}
}
