package audit

// 降智观测面（纯 KPI，不参与任何执行——执行在网关零延迟拦截）。
// 历史上的 soft_tps/hard_tps/buffered_burst 速率档位是 TPS 时代守卫的
// 遗物：零延迟拦截落地后不再有按速率的执行，档位也没有任何 UI/汇总
// 消费方，随重构删除。保留的唯一档位 terminal_burst 用于让「速度列为
// 空」的最强降智签名在审计明细里可见。
const (
	DegradeClassTerminalBurst = "terminal_burst"
	ErrorQualityDegraded      = "quality_degraded"
)

const (
	DefaultDegradeMinGenMS  = int64(1000)
	DefaultDegradeMinOutput = int64(32)
)

// ClassifyTerminalBurst reports whether a successful streaming row carries
// the terminal-burst degrade signature: the whole output arrived in one final
// chunk (first token >= total duration) with output at or above the minimum
// sighting threshold. Rows like this have no meaningful token/s (division by
// ~0ms), so the throughput column shows empty - this class is what keeps the
// signature visible (线上续聊链连续降智正是这个形态，此前在
// 一切速率汇总里反而隐形)。
func ClassifyTerminalBurst(outputTokens, reasoningTokens, firstTokenMS, durationMS int64) bool {
	if durationMS <= 0 || outputTokens < DefaultDegradeMinOutput {
		return false
	}
	return GenerationWindowMS(firstTokenMS, durationMS, reasoningTokens) <= 0
}

// GenerationWindowMS is the Token/s denominator shared by the audit panel,
// dashboard, probes, and quality guard.
//
// Normally that is duration - first token. Older audit rows may have measured
// first token only when buffered reasoning was finally flushed. For rows that
// actually report reasoning tokens, use the full duration when the remaining
// tail is implausibly short. Rows without reasoning evidence retain the tail
// so real buffered output bursts stay visible in the throughput column.
func GenerationWindowMS(firstTokenMS, durationMS, reasoningTokens int64) int64 {
	if durationMS <= 0 {
		return 0
	}
	if firstTokenMS < 0 {
		firstTokenMS = 0
	}
	if firstTokenMS >= durationMS {
		return 0
	}
	generationMS := durationMS - firstTokenMS
	if reasoningTokens > 0 && generationMS < firstTokenMS && generationMS < DefaultDegradeMinGenMS {
		return durationMS
	}
	return generationMS
}

func OutputTokensPerSecond(outputTokens, reasoningTokens, firstTokenMS, durationMS int64) float64 {
	generationMS := GenerationWindowMS(firstTokenMS, durationMS, reasoningTokens)
	if outputTokens <= 0 || generationMS <= 0 {
		return 0
	}
	return float64(outputTokens) * 1000 / float64(generationMS)
}
