package accountsync

import (
	"context"
	"sync"
	"sync/atomic"
)

const accountSyncQueueCapacity = 20

type Synchronizer interface {
	Sync(ctx context.Context, accountIDs ...uint64) Result
	SyncStream(ctx context.Context, accountIDs <-chan uint64) Result
}
type syncProgressor interface {
	SyncStreamObserved(ctx context.Context, accountIDs <-chan uint64, observer func(completed, total int)) Result
}

type accountSyncPipeline struct {
	ctx        context.Context
	cancel     context.CancelFunc
	ids        chan uint64
	done       chan Result
	progress   func(completed, total int)
	progressMu sync.Mutex
	queued     atomic.Int64
	completed  atomic.Int64
}

func startSyncPipeline(parent context.Context, syncer Synchronizer, progress func(completed, total int)) *accountSyncPipeline {
	ctx, cancel := context.WithCancel(parent)
	pipeline := &accountSyncPipeline{ctx: ctx, cancel: cancel, progress: progress}
	if syncer == nil {
		return pipeline
	}
	pipeline.ids = make(chan uint64, accountSyncQueueCapacity)
	pipeline.done = make(chan Result, 1)
	go func() {
		if observed, ok := syncer.(syncProgressor); ok && progress != nil {
			pipeline.done <- observed.SyncStreamObserved(ctx, pipeline.ids, func(completed, _ int) {
				pipeline.completed.Store(int64(completed))
				pipeline.reportProgress()
			})
			return
		}
		pipeline.done <- syncer.SyncStream(ctx, pipeline.ids)
	}()
	return pipeline
}

func (p *accountSyncPipeline) Observe(accountID uint64) error {
	if p.ids == nil {
		return nil
	}
	p.queued.Add(1)
	select {
	case p.ids <- accountID:
		return nil
	case <-p.ctx.Done():
		p.queued.Add(-1)
		return p.ctx.Err()
	}
}

func (p *accountSyncPipeline) Finish(abort bool) Result {
	if abort {
		p.cancel()
	}
	if p.ids != nil {
		close(p.ids)
	}
	if !abort {
		// 转换阶段结束后不再增加同步任务，先报告一次固定分母，避免前端看到总数跳变。
		p.reportProgress()
	}
	result := Result{}
	if p.done != nil {
		result = <-p.done
	}
	if !abort && p.ids != nil {
		p.completed.Store(int64(result.Succeeded + result.Failed))
		p.reportProgress()
	}
	p.cancel()
	return result
}

// reportProgress 使用已进入同步流水线的任务数报告进度；转换结束后该分母保持固定。
func (p *accountSyncPipeline) reportProgress() {
	if p.ids == nil || p.progress == nil {
		return
	}
	p.progressMu.Lock()
	defer p.progressMu.Unlock()
	p.progress(int(p.completed.Load()), int(p.queued.Load()))
}
