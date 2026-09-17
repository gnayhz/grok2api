package gateway

import qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"

// builtinQualityKernel has no process-wide mutable installation state.
// The application may replace it for an individual gateway snapshot.
type builtinQualityKernel struct{}

func (builtinQualityKernel) ClassifyQualityHold(sig QualityStreamSignals) QualityVerdict {
	verdict, _ := qualityguard.Judge(qualityHoldSignals(sig))
	return QualityVerdict(verdict)
}

// qualityHoldSignals 是网关信号到守卫信号的唯一映射;判决入口与规则
// 指纹查询共用,避免字段漂移导致"判决与解释不一致"。
func qualityHoldSignals(sig QualityStreamSignals) qualityguard.Signals {
	return qualityguard.Signals{
		HasThinking: sig.HasThinking, ReasoningEndedWithoutThinking: sig.ReasoningEndedWithoutThinking,
		VisibleTokens: sig.VisibleTokens, OutputTokens: sig.OutputTokens, Terminal: sig.Terminal,
	}
}

// classifyQualityHoldShadowed retains the scanner test entry point. It is pure.
func classifyQualityHoldShadowed(sig QualityStreamSignals) QualityVerdict {
	return builtinQualityKernel{}.ClassifyQualityHold(sig)
}
func classifyQualityHold(cfg QualityRetryRuntime, sig QualityStreamSignals) QualityVerdict {
	if cfg.Kernel() != nil {
		return cfg.Kernel().ClassifyQualityHold(sig)
	}
	return classifyQualityHoldShadowed(sig)
}
