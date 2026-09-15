package gateway

import (
	"bytes"
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	"io"
	"sync"
	"time"
)

type qualityReadResult struct {
	data []byte
	err  error
}

// qualityReadPump is the sole reader of the upstream body. It lets the hold
// timer win while an upstream Read is blocked, then remains the continuation
// reader for the response body after the held prefix is replayed.
type qualityReadPump struct {
	source      io.ReadCloser
	results     chan qualityReadResult
	done        chan struct{}
	exited      chan struct{}
	closeOnce   sync.Once
	pending     []byte
	finalErr    error
	bufferLease *responsebuffer.Lease
	readMu      sync.Mutex
}

func newQualityReadPump(source io.ReadCloser) *qualityReadPump {
	return newQualityReadPumpWithBudget(source, responsebuffer.BudgetOf(source))
}

func newQualityReadPumpWithBudget(source io.ReadCloser, budget *responsebuffer.Budget) *qualityReadPump {
	pump := &qualityReadPump{
		source:  source,
		results: make(chan qualityReadResult),
		done:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
	lease, err := budget.Reserve(2 * qualityReadChunkBytes)
	if err != nil {
		pump.results = make(chan qualityReadResult, 1)
		pump.results <- qualityReadResult{err: err}
		close(pump.results)
		close(pump.exited)
		return pump
	}
	pump.bufferLease = lease
	go pump.run()
	return pump
}

func (p *qualityReadPump) run() {
	defer close(p.exited)
	defer close(p.results)
	// Double-buffer so the consumer can keep result.data without a copy:
	// send is synchronous, and the next Read uses the other backing array.
	bufs := [2][]byte{
		make([]byte, qualityReadChunkBytes),
		make([]byte, qualityReadChunkBytes),
	}
	which := 0
	emptyReads := 0
	for {
		select {
		case <-p.done:
			return
		default:
		}
		buf := bufs[which]
		n, err := p.source.Read(buf)
		if n == 0 && err == nil {
			emptyReads++
			if emptyReads < 100 {
				continue
			}
			err = io.ErrNoProgress
		}
		emptyReads = 0
		result := qualityReadResult{err: err}
		if n > 0 {
			result.data = buf[:n]
		}
		select {
		case p.results <- result:
		case <-p.done:
			return
		}
		if err != nil {
			return
		}
		which ^= 1
	}
}

func (p *qualityReadPump) Read(dst []byte) (int, error) {
	p.readMu.Lock()
	defer p.readMu.Unlock()
	if len(dst) == 0 {
		return 0, nil
	}
	for len(p.pending) == 0 {
		if p.finalErr != nil {
			return 0, p.finalErr
		}
		result, ok := <-p.results
		if !ok {
			p.finalErr = io.EOF
			return 0, io.EOF
		}
		p.pending = result.data
		p.finalErr = result.err
		if len(p.pending) == 0 && p.finalErr != nil {
			return 0, p.finalErr
		}
	}
	n := copy(dst, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *qualityReadPump) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.done)
		err = p.source.Close()
		<-p.exited
		p.readMu.Lock()
		p.pending = nil
		p.bufferLease.Release()
		p.readMu.Unlock()
	})
	return err
}

func peekQualityStream(ctx context.Context, body io.ReadCloser, protocol string, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, error) {
	replay, verdict, usage, _, err := peekQualityStreamReport(ctx, body, protocol, cfg)
	return replay, verdict, usage, err
}

func peekQualityStreamReport(ctx context.Context, body io.ReadCloser, protocol string, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, qualityHoldFingerprint, error) {
	if stream := responseflow.FromReader(body); stream != nil {
		return peekCanonicalQualityStream(ctx, body, stream, protocol, cfg)
	}
	cfg = normalizeQualityRetry(cfg)
	empty := qualityScanState{protocol: protocol, startedAt: time.Now()}
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, empty.fingerprint(QualityWait, errQualityEmptyStream), errQualityEmptyStream
	}
	if ctx.Err() != nil {
		_ = body.Close()
		err := qualityPeekAbortError(ctx, ctx.Err())
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, empty.fingerprint(QualityWait, err), err
	}
	pump := newQualityReadPumpWithBudget(body, responsebuffer.FromContext(ctx))
	state := qualityScanState{kernel: cfg.kernel, protocol: protocol, startedAt: time.Now()}
	held := responsebuffer.New(responsebuffer.FromContext(ctx), qualityHoldMaxBufferBytes)
	frameOffset := 0
	liveness := newQualityLivenessTimer(cfg)
	defer liveness.timer.Stop()
	emit := func(replay io.ReadCloser, verdict QualityVerdict, usage Usage, err error) (io.ReadCloser, QualityVerdict, Usage, qualityHoldFingerprint, error) {
		if ctx.Err() != nil {
			verdict, err = QualityWait, qualityPeekAbortError(ctx, ctx.Err())
		}
		if verdict != QualityDeliver || err != nil {
			_ = pump.Close()
			if prefix, ok := replay.(*qualityPrefixReplay); ok {
				prefix.rest = nil
			}
		}
		return replay, verdict, usage, state.fingerprint(verdict, err), err
	}
	for {
		if verdict, err := state.streamVerdict(cfg.ReasoningExpected); verdict != QualityWait || err != nil {
			return emit(newPrefixReplay(held, pump), verdict, state.usage, err)
		}

		select {
		case <-ctx.Done():
			_ = pump.Close()
			return emit(held.Body(), QualityWait, state.usage, qualityPeekAbortError(ctx, ctx.Err()))
		case <-liveness.timer.C:
			// Timeouts are inconclusive. Useful output retains its existing
			// evidence rules and the request-wide admission deadline.
			if timeout := liveness.timeout(); timeout != nil && !state.hasThinking && state.visibleRunes == 0 && state.aggregateRunes == 0 && !state.semanticOutput {
				_ = pump.Close()
				return emit(held.Body(), QualityWait, state.usage, timeout)
			}
		case result, ok := <-pump.results:
			if !ok {
				replay, verdict, usage, err := finishQualityPeek(held, pump, &state, cfg)
				return emit(replay, verdict, usage, err)
			}
			pump.finalErr = result.err // Preserve Read(n>0, err) on healthy replay.
			if len(result.data) > 0 {
				// Admit only the remaining budget, then classify complete events in order.
				// A valid early decision within the budget still wins over a large read.
				remaining := qualityHoldMaxBufferBytes - held.Len()
				admitted := result.data[:min(remaining, len(result.data))]
				searched := held.Len() - frameOffset
				if _, err := held.Write(admitted); err != nil {
					if errors.Is(err, responsebuffer.ErrLimit) {
						err = errQualityHoldLimit
					}
					return emit(newPrefixReplay(held, pump), QualityWait, state.usage, err)
				}
				sawData := state.sawDataEvent
				consumed, verdict, scanErr := scanQualityLines(&state, held.Bytes()[frameOffset:], searched, &cfg)
				frameOffset += consumed
				state.pending = held.Bytes()[frameOffset:] // Borrow only; EOF may complete this final line.
				if !sawData && state.sawDataEvent {
					if err := liveness.observeData(); err != nil {
						return emit(newPrefixReplay(held, pump), QualityWait, state.usage, err)
					}
				}
				if verdict != QualityWait || scanErr != nil {
					// Preserve the unobserved suffix for a healthy response's exact replay.
					if verdict == QualityDeliver {
						pump.pending = result.data[len(admitted):]
					}
					return emit(newPrefixReplay(held, pump), verdict, state.usage, scanErr)
				}
				if len(admitted) < len(result.data) {
					return emit(newPrefixReplay(held, pump), QualityWait, state.usage, errQualityHoldLimit)
				}
			}

			if result.err == io.EOF {
				replay, verdict, usage, err := finishQualityPeek(held, pump, &state, cfg)
				return emit(replay, verdict, usage, err)
			}
			if result.err != nil {
				_ = pump.Close()
				return emit(held.Body(), QualityWait, state.usage, qualityPeekAbortError(ctx, result.err))
			}
		}
	}
}

func finishQualityPeek(held *responsebuffer.Buffer, pump *qualityReadPump, state *qualityScanState, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, error) {
	if state == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, errQualityEmptyStream
	}
	if len(state.pending) > 0 {
		// A complete final JSON event is accepted without a trailing newline.
		observeQualityLine(state, state.pending)
		state.pending = nil
	}

	state.terminal = true
	signals := state.signals()
	// 空流判定只看流证据：只有 usage 帧（声称 reasoning tokens）而零内容零
	// 推理事件的流同样是空流——usage 声明不能把空 200 洗成可投递响应。
	// 纯语义输出（工具调用等）不是空流：思考期望内按扣留收口，其余交付。
	if state.emptyEvidence() {
		verdict, verdictErr := state.emptyStreamVerdict(cfg.ReasoningExpected)
		return newPrefixReplay(held, pump), verdict, state.usage, verdictErr
	}
	return newPrefixReplay(held, pump), cfg.classify(signals), state.usage, nil
}

func newPrefixReplay(held *responsebuffer.Buffer, rest io.ReadCloser) io.ReadCloser {
	return &qualityPrefixReplay{prefix: held.Body(), rest: rest}
}

type qualityPrefixReplay struct {
	prefix     *responsebuffer.Body
	rest       io.ReadCloser
	prefixDone bool
}

func (b *qualityPrefixReplay) ResponseBudget() *responsebuffer.Budget {
	return b.prefix.ResponseBudget()
}
func (b *qualityPrefixReplay) Read(p []byte) (int, error) {
	if !b.prefixDone {
		n, err := b.prefix.Read(p)
		if err != io.EOF {
			return n, err
		}
		b.prefixDone = true
		_ = b.prefix.Close()
	}
	if b.rest == nil {
		return 0, io.EOF
	}
	return b.rest.Read(p)
}
func (b *qualityPrefixReplay) Close() error {
	err := b.prefix.Close()
	if b.rest != nil {
		err = errors.Join(err, b.rest.Close())
	}
	return err
}
