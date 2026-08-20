package audit

const ErrorQualityDegraded = "quality_degraded"

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
	if reasoningTokens > 0 && generationMS < firstTokenMS && generationMS < 1000 {
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
