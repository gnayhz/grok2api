package provider

import (
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// RejectionClassifier 把上游状态/错误体解释为稳定的凭据拒绝事实。
// 方言级 JSON/text 解释属于 Provider 边界；应用层消费方经构造注入本实现。
type RejectionClassifier struct{}

func NewRejectionClassifier() *RejectionClassifier { return &RejectionClassifier{} }

func (RejectionClassifier) ClassifyCredentialRejection(status int, body []byte, err error) provider.CredentialRejection {
	return provider.ClassifyCredentialRejection(status, body, err)
}
