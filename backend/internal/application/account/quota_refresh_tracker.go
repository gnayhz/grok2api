package account

import "sync"

// quotaRefreshTracker 持有额度刷新协调的全部可变状态:观察表、互斥、
// 有界队列与调度唤醒通道(QuotaBilling 状态面)。Service 只经组件
// 字段访问;队列消费/恢复编排在 quota_refresh_queue.go。
type quotaRefreshTracker struct {
	mu    sync.Mutex
	obs   map[string]*quotaRefreshState
	queue chan quotaRefreshRequest
	wake  chan struct{}
}
