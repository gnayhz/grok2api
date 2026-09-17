package provider

import (
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// RateLimitInterpreter 把上游 429 状态/头/正文解释为 RateLimitMetadata 事实。
// 方言级正则与文本形状解析属于 Provider 边界;应用层消费方经构造注入。
type RateLimitInterpreter struct{}

func NewRateLimitInterpreter() *RateLimitInterpreter { return &RateLimitInterpreter{} }

func (RateLimitInterpreter) FromResponse(status int, header http.Header, body []byte) *provider.RateLimitMetadata {
	return provider.RateLimitFromResponse(status, header, body)
}

func (RateLimitInterpreter) ParseRateLimitMetadata(body []byte) *provider.RateLimitMetadata {
	return provider.ParseRateLimitMetadata(body)
}
