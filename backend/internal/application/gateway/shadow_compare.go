package gateway

import qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"

// builtinQualityKernel has no process-wide mutable installation state.
// The application may replace it for an individual gateway snapshot.
type builtinQualityKernel struct{}

func (builtinQualityKernel) ClassifyQualityHold(sig QualityStreamSignals) QualityVerdict {
	verdict, _ := qualityguard.Judge(qualityguard.Signals{
		HasThinking: sig.HasThinking, ReasoningEndedWithoutThinking: sig.ReasoningEndedWithoutThinking,
		VisibleTokens: sig.VisibleTokens, OutputTokens: sig.OutputTokens, Terminal: sig.Terminal,
	})
	return QualityVerdict(verdict)
}

// classifyQualityHoldShadowed retains the scanner test entry point. It is pure.
func classifyQualityHoldShadowed(sig QualityStreamSignals) QualityVerdict {
	return builtinQualityKernel{}.ClassifyQualityHold(sig)
}
func (cfg QualityRetryRuntime) classify(sig QualityStreamSignals) QualityVerdict {
	if cfg.kernel != nil {
		return cfg.kernel.ClassifyQualityHold(sig)
	}
	return classifyQualityHoldShadowed(sig)
}
