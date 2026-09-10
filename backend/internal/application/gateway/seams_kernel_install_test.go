package gateway

// 测试期判决内核安装(B4 决议2 缝隙化的测试侧):网关测试大量直接
// 调用 classifyQualityHoldShadowed 断言三规则判决——安装真实内核
// (quality/guard.Judge 经适配器)保持断言语义不变。仅 _test.go 引用
// 质量层,底座非测试代码零质量层 import(D2 可剥离性作用于产物)。

import (
	qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"
)

type guardKernelAdapter struct{}

func (guardKernelAdapter) ClassifyQualityHold(sig QualityStreamSignals) QualityVerdict {
	verdict, _ := qualityguard.Judge(qualityguard.Signals{
		HasThinking:                   sig.HasThinking,
		ReasoningEndedWithoutThinking: sig.ReasoningEndedWithoutThinking,
		VisibleTokens:                 sig.VisibleTokens,
		OutputTokens:                  sig.OutputTokens,
		Terminal:                      sig.Terminal,
	})
	switch verdict {
	case qualityguard.Deliver:
		return QualityDeliver
	case qualityguard.Withhold:
		return QualityWithhold
	default:
		return QualityWait
	}
}
