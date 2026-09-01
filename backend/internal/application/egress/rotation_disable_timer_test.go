package egress

import (
	"testing"
	"time"
)

func queueLength(s *Service) int {
	if s == nil || s.rotation == nil {
		return 0
	}
	s.rotation.mu.Lock()
	defer s.rotation.mu.Unlock()
	return len(s.rotation.queue)
}

// 回归锁(P2,契约违反):SetRotationConfig 承诺"Disabled config drops any
// queued work"。但 requeueAfter 的 time.AfterFunc 定时器在禁用时仍挂起——
// 到点后 requeue 把节点送回队列,禁用前已挂起的轮换会在禁用后重新排队。
// worker 下一次(重新启用或任何路径)消费到它时,会对管理员明确关闭轮换的
// 节点发起 webhook。禁用语义必须同时清除挂起定时器的回流。
func TestSetRotationConfigDisabledDropsPendingRequeueTimers(t *testing.T) {
	scheduler := &rotationScheduler{set: make(map[uint64]struct{}), wake: make(chan struct{}, 1)}
	service := &Service{rotation: scheduler}
	service.SetRotationConfig(fastRotationConfig())

	// 模拟限速/最小间隔触发的挂起重排(短延迟,足以跨过禁用时刻)。
	service.rotation.requeueAfter(42, 30*time.Millisecond)
	if queueLength(service) != 0 {
		t.Fatalf("requeueAfter must defer, immediate queue length = %d", queueLength(service))
	}

	// 禁用:契约承诺丢弃全部排队工作。
	disabled := fastRotationConfig()
	disabled.Enabled = false
	service.SetRotationConfig(disabled)

	// 等待挂起定时器越过禁用时刻到点。
	time.Sleep(120 * time.Millisecond)

	if got := queueLength(service); got != 0 {
		t.Fatalf("pending requeue timer resurrected a node after rotation was disabled: queue=%d", got)
	}
}
