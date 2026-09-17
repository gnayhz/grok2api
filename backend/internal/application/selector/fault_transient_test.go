package selector

import (
	"context"
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

// TestTransientRoutingStoreFailureClassOnlySkipsTransientFaults 锁定"确定性
// 存储故障不得按单号跳过"的合同：约束冲突与未归类故障是确定性结果，把它们
// 当作瞬态会把一次永久失败放大成对后续候选的连续无效尝试（最多 8 次换号），
// 并让最终错误信息指向错误的失败类别。
func TestTransientRoutingStoreFailureClassOnlySkipsTransientFaults(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		err           error
		wantClass     string
		wantTransient bool
	}{
		{name: "nil", err: nil, wantClass: "", wantTransient: false},
		{name: "context canceled", err: context.Canceled, wantClass: "", wantTransient: false},
		{name: "wrapped context canceled", err: errors.Join(errors.New("load"), context.Canceled), wantClass: "", wantTransient: false},
		{name: "deadline", err: context.DeadlineExceeded, wantClass: "deadline", wantTransient: true},
		{name: "connection fault", err: &repository.StoreFault{Kind: repository.StoreFaultConnection}, wantClass: "store:connection", wantTransient: true},
		{name: "lock fault", err: &repository.StoreFault{Kind: repository.StoreFaultLock}, wantClass: "store:lock", wantTransient: true},
		{name: "serialization fault", err: &repository.StoreFault{Kind: repository.StoreFaultSerialization}, wantClass: "store:serialization", wantTransient: true},
		{name: "timeout fault", err: &repository.StoreFault{Kind: repository.StoreFaultTimeout}, wantClass: "store:timeout", wantTransient: true},
		{name: "constraint fault is deterministic", err: &repository.StoreFault{Kind: repository.StoreFaultConstraint}, wantClass: "", wantTransient: false},
		{name: "unknown fault is not transient", err: &repository.StoreFault{Kind: repository.StoreFaultUnknown}, wantClass: "", wantTransient: false},
		{name: "sentinel conflict", err: repository.ErrConflict, wantClass: "", wantTransient: false},
		{name: "unclassified error", err: errors.New("boom"), wantClass: "", wantTransient: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			class, transient := transientRoutingStoreFailureClass(testCase.err)
			if class != testCase.wantClass || transient != testCase.wantTransient {
				t.Fatalf("transientRoutingStoreFailureClass(%v) = (%q, %v), want (%q, %v)",
					testCase.err, class, transient, testCase.wantClass, testCase.wantTransient)
			}
		})
	}
}

// TestStaleSnapshotAndSkipShareOneFaultWhitelist 锚定两个消费方使用同一条
// 类别白名单：旧快照回退与按单号跳过曾经的取舍规则相反，导致同一个确定性
// 约束故障既不允许读旧快照、却允许跳过账号。
func TestStaleSnapshotAndSkipShareOneFaultWhitelist(t *testing.T) {
	for _, kind := range []repository.StoreFaultKind{
		repository.StoreFaultConnection, repository.StoreFaultLock,
		repository.StoreFaultSerialization, repository.StoreFaultTimeout,
		repository.StoreFaultConstraint, repository.StoreFaultUnknown,
	} {
		fault := &repository.StoreFault{Kind: kind, Cause: errors.New("driver")}
		stale := canUseStaleRoutingSnapshot(context.Background(), fault)
		_, skippable := transientRoutingStoreFailureClass(fault)
		if stale != skippable {
			t.Fatalf("kind %q: stale-snapshot=%v but skip=%v; the two consumers must agree", kind, stale, skippable)
		}
		if stale != repository.StoreFaultTransient(kind) {
			t.Fatalf("kind %q: consumer verdict disagrees with repository.StoreFaultTransient", kind)
		}
	}
}
