package egress

import (
	"testing"
)

// TestAttachQualityStatesSeam 锚定批8 可见性整改:质量状态经缝隙旁注到
// 节点响应(remanded/banned + 案件号);缝隙未注入(剥离形态)时节点
// 响应不含质量字段(D2)。
func TestAttachQualityStatesSeam(t *testing.T) {
	handler := &Handler{}
	items := []nodeResponse{{ID: 7}, {ID: 9}}
	// 剥离形态:未注入 → 字段缺席。
	handler.attachQualityStates(items)
	if items[0].Quality != nil {
		t.Fatal("未注入缝隙时不得旁注质量字段")
	}
	// 注入形态:仅被押/被禁节点带状态。
	handler.SetQualityStates(func() map[uint64]NodeQualityState {
		return map[uint64]NodeQualityState{
			7: {State: "remanded", CaseID: 42},
			9: {State: "banned"},
		}
	})
	handler.attachQualityStates(items)
	if items[0].Quality == nil || items[0].Quality.State != "remanded" || items[0].Quality.CaseID != 42 {
		t.Fatalf("羁押状态未旁注: %+v", items[0].Quality)
	}
	if items[1].Quality == nil || items[1].Quality.State != "banned" {
		t.Fatalf("禁用状态未旁注: %+v", items[1].Quality)
	}
	// 空状态(全部可用)no-op。
	handler.SetQualityStates(func() map[uint64]NodeQualityState { return nil })
	fresh := []nodeResponse{{ID: 7}}
	handler.attachQualityStates(fresh)
	if fresh[0].Quality != nil {
		t.Fatal("全可用时不得旁注质量字段")
	}
}
