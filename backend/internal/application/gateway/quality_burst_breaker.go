package gateway

import (
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// terminalBurstWindow 是连击计数的时间窗：窗口外的上一次爆发不计入连击
// （跨窗口的零星爆发更可能是测量噪声而非持续降智）。
const terminalBurstWindow = 30 * time.Minute

// terminalBurstTracker 记录每账号"整包末尾爆发+零思考"交付的连续次数。
// 它是流级质量守卫之外的纵深防御：守卫扣留路径修复后，残余放行面
// （豁免请求/超大 body fail-open/重试耗尽 deliver_last）仍可能把降智响应
// 交给客户端；同账号连续 N 次同签名交付即按 missing-thinking 语义处罚，
// 避免 2026-08-27 线上"续聊链 7 连发零惩罚"重演。计数为进程内存，重启
// 归零——处罚本身是持久化的，计数只做触发。
type terminalBurstTracker struct {
	mu      sync.Mutex
	entries map[uint64]*terminalBurstEntry
}

type terminalBurstEntry struct {
	count    int
	lastSeen time.Time
}

func newTerminalBurstTracker() *terminalBurstTracker {
	return &terminalBurstTracker{entries: make(map[uint64]*terminalBurstEntry)}
}

// observeBurst 记录一次爆发签名交付，返回该账号当前的连续计数。
func (t *terminalBurstTracker) observeBurst(accountID uint64) int {
	if t == nil || accountID == 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	entry, ok := t.entries[accountID]
	if !ok || now.Sub(entry.lastSeen) > terminalBurstWindow {
		entry = &terminalBurstEntry{}
		t.entries[accountID] = entry
	}
	entry.count++
	entry.lastSeen = now
	return entry.count
}

// observeHealthy 记录一次带思考证据的健康交付，清零该账号连击：只有
// 明确的健康信号（上游报告思考 token）才解除计数，模糊情形不重置。
func (t *terminalBurstTracker) observeHealthy(accountID uint64) {
	if t == nil || accountID == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, accountID)
}

// reset 在熔断触发并完成处罚后清零连击，下一轮从零重新累计。
func (t *terminalBurstTracker) reset(accountID uint64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, accountID)
}

// terminalBurstSignature 判定一条已交付的流式结果是否是"整包末尾爆发+
// 零思考"降智签名：首字节时间不小于总时长（全部输出在流末尾一次到达，
// 生成窗口≈0）、上游报告零思考 token、输出达到降智最小口径（32）。
// 2026-08-27 线上续聊链 7 条事故行全部精确命中该签名（out=339/94、
// reason=0、first==dur）；健康流式交付因思考增量早于流结束而天然排除。
func terminalBurstSignature(firstTokenMS *int64, durationMS, reasoningTokens, outputTokens int64) bool {
	if firstTokenMS == nil || durationMS <= 0 || reasoningTokens != 0 {
		return false
	}
	if outputTokens < audit.DefaultDegradeMinOutput {
		return false
	}
	return *firstTokenMS >= durationMS
}
