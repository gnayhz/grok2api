package provider

import (
	"io"

	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// ResponsesProbeInspector 把账号可用性检测的原生 Responses 结果解释为
// 拒绝事实/生成完成判定。方言级 JSON 读取与完成规则在 Provider 边界;
// 应用层消费方经构造注入(与 RejectionClassifier 同一合同面)。
type ResponsesProbeInspector struct{}

func NewResponsesProbeInspector() *ResponsesProbeInspector { return &ResponsesProbeInspector{} }

func (ResponsesProbeInspector) InspectResponsesProbe(status int, body io.Reader) (provider.CredentialRejection, error) {
	return provider.InspectResponsesProbe(status, body)
}
