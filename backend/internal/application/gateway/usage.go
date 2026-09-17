package gateway

import "math"

// Usage 的派生口径(total 回退、Anthropic 输入重算)归 gateway 所有;
// 传输层只做协议 DTO 字段解码,不在此处另立规则。

// WithTotalFallback 在上游未报告 total 时以 input+output 饱和回退。
func (u Usage) WithTotalFallback() Usage {
	if u.TotalTokens == 0 {
		u.TotalTokens = SaturatingUsageSum(u.InputTokens, u.OutputTokens)
	}
	return u
}

// RecomputeAnthropicInput 是 Anthropic Messages 协议的输入计入口径:
// input = input + cache_read + cache_creation(饱和加法,只计正值),
// total 随之重算。Anthropic 把缓存写入计入 input 计费,忽略该口径会
// 低估用量与费用。
func (u Usage) RecomputeAnthropicInput(cacheCreationInputTokens int64) Usage {
	input := SaturatingUsageSum(u.InputTokens, u.CachedInputTokens, cacheCreationInputTokens)
	u.InputTokens = input
	u.TotalTokens = SaturatingUsageSum(input, u.OutputTokens)
	return u
}

// SaturatingUsageSum 求和,忽略非正值,溢出时饱和到 MaxInt64。
func SaturatingUsageSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if value > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += value
	}
	return total
}
