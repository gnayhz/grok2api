package gateway

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
)

// mediaBackground 持有媒体后台执行的全部可变状态：持久任务队列、去重
// 集合、固定 worker 数、输入物化槽位与额度恢复游标。gateway 只通过本
// 组件的方法接触这些状态；worker 生命周期独立（Run 可重入拒绝），处理
// 逻辑由注入的 processor 回调提供，关闭由调用方 ctx 排空。
type mediaBackground struct {
	queue      chan string
	queued     map[string]struct{}
	mu         sync.Mutex
	workers    int
	inputSlots chan struct{}
	queueFull  atomic.Uint64

	quotaMu     sync.Mutex
	quotaCursor string

	logger   interface{ Warn(string, ...any) }
	errorLog interface{ Error(string, ...any) }
	process  func(ctx context.Context, id string)

	runningMu sync.Mutex
	running   bool
}

func newMediaBackground(concurrency int, logger interface{ Warn(string, ...any) }, errorLog interface{ Error(string, ...any) }) *mediaBackground {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &mediaBackground{
		queue:      make(chan string, min(2048, max(64, concurrency*32))),
		queued:     make(map[string]struct{}),
		workers:    concurrency,
		inputSlots: make(chan struct{}, min(concurrency, videoInputMaterializeConcurrency)),
		logger:     logger,
		errorLog:   errorLog,
	}
}

// SetProcessor 安装任务处理器（构造后、Run 前调用一次）。
func (b *mediaBackground) SetProcessor(process func(ctx context.Context, id string)) {
	b.process = process
}

// Run 启动固定 worker 池；重复 Run 返回 false 不产生第二组 goroutine。
// ctx 结束后 worker 排空退出，Run 返回。
func (b *mediaBackground) Run(ctx context.Context) bool {
	b.runningMu.Lock()
	if b.running {
		b.runningMu.Unlock()
		return false
	}
	b.running = true
	b.runningMu.Unlock()
	var wg sync.WaitGroup
	wg.Add(b.workers)
	for range b.workers {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-b.queue:
					err := batch.Do(ctx, func(workCtx context.Context) error {
						if b.process != nil {
							b.process(workCtx, id)
						}
						return nil
					})
					b.mu.Lock()
					delete(b.queued, id)
					b.mu.Unlock()
					if err != nil && ctx.Err() == nil {
						if panicErr, ok := err.(*batch.PanicError); ok {
							if b.errorLog != nil {
								b.errorLog.Error("video_worker_panicked", "job_id", id, "error", panicErr, "stack", string(panicErr.Stack))
							}
						} else if b.errorLog != nil {
							b.errorLog.Error("video_worker_failed", "job_id", id, "error", err)
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	b.runningMu.Lock()
	b.running = false
	b.runningMu.Unlock()
	return true
}

// Enqueue 去重入队；队列满时丢弃并按节流记录告警。
func (b *mediaBackground) Enqueue(id string) bool {
	if id == "" || b.queue == nil {
		return false
	}
	b.mu.Lock()
	if _, exists := b.queued[id]; exists {
		b.mu.Unlock()
		return true
	}
	b.queued[id] = struct{}{}
	b.mu.Unlock()
	select {
	case b.queue <- id:
		return true
	default:
		b.mu.Lock()
		delete(b.queued, id)
		b.mu.Unlock()
		full := b.queueFull.Add(1)
		if b.logger != nil && (full == 1 || full%100 == 0) {
			b.logger.Warn("video_queue_full", "count", full, "queued", len(b.queue), "capacity", cap(b.queue))
		}
		return false
	}
}

// AcquireInputSlot 物化输入引用时占用受限槽位；返回的释放函数可幂等
// 重复调用(重复释放不得吃掉其他请求的槽位令牌)。
func (b *mediaBackground) AcquireInputSlot(ctx context.Context) (func(), error) {
	if b.inputSlots == nil {
		return func() {}, nil
	}
	select {
	case b.inputSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-b.inputSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// beginQuotaReconcile 非阻塞占用额度恢复轮转;另一轮恢复在执行时返回
// finish=nil。占用期间游标只能经返回的 advance/reset 推进,quotaMu 与
// quotaCursor 不再被组件外的代码触碰。
func (b *mediaBackground) beginQuotaReconcile() (cursor string, advance func(string), reset func(), finish func()) {
	if !b.quotaMu.TryLock() {
		return "", nil, nil, nil
	}
	return b.quotaCursor,
		func(id string) { b.quotaCursor = id },
		func() { b.quotaCursor = "" },
		b.quotaMu.Unlock
}
