package repository

import (
	"errors"
	"testing"
)

// TestStoreFaultTransientIsTheSingleClassification 锁定瞬态类别的唯一定义。
// 消费方（旧快照回退、按单号跳过、重试）不得各自维护白名单：约束冲突是
// 确定性结果，未归类故障语义未知，二者都不是瞬态。
func TestStoreFaultTransientIsTheSingleClassification(t *testing.T) {
	for _, testCase := range []struct {
		kind StoreFaultKind
		want bool
	}{
		{StoreFaultConnection, true},
		{StoreFaultLock, true},
		{StoreFaultSerialization, true},
		{StoreFaultTimeout, true},
		{StoreFaultConstraint, false},
		{StoreFaultUnknown, false},
		{StoreFaultKind(""), false},
		{StoreFaultKind("future-kind"), false},
	} {
		if got := StoreFaultTransient(testCase.kind); got != testCase.want {
			t.Errorf("StoreFaultTransient(%q) = %v, want %v", testCase.kind, got, testCase.want)
		}
	}
}

// TestStoreFaultKindOfPreservesKindThroughWrapping 确认分类可穿透包装链，
// 消费方不需要自行 errors.As。
func TestStoreFaultKindOfPreservesKindThroughWrapping(t *testing.T) {
	wrapped := errors.Join(errors.New("load credential material"), &StoreFault{Kind: StoreFaultLock, Cause: errors.New("database is locked")})
	kind, ok := StoreFaultKindOf(wrapped)
	if !ok || kind != StoreFaultLock {
		t.Fatalf("StoreFaultKindOf(wrapped) = (%q, %v), want (%q, true)", kind, ok, StoreFaultLock)
	}
	if !StoreFaultTransient(kind) {
		t.Fatalf("wrapped lock fault must stay transient")
	}
	if _, ok := StoreFaultKindOf(errors.New("plain")); ok {
		t.Fatal("plain error must not classify as a store fault")
	}
}
