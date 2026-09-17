package provider

import (
	"bytes"
	"fmt"
	"io"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
)

// InspectResponsesProbe interprets the native Responses result of an account
// availability check. Explicit credential/quota refusals remain separate from
// unreadable, truncated or incomplete generation; the caller owns state changes.
// The caller closes body on every exit. An arbitrary HTTP 2xx is not completion.
func InspectResponsesProbe(status int, body io.Reader) (CredentialRejection, error) {
	data, truncated, err := ReadDiagnosticBody(body)
	rejection := ClassifyCredentialRejection(status, data, nil)
	if err != nil {
		return rejection, fmt.Errorf("读取账号检测响应失败: %w", err)
	}
	if truncated {
		return rejection, fmt.Errorf("账号检测响应超过读取上限")
	}
	if status < 200 || status >= 300 {
		return rejection, nil
	}
	output := bytes.TrimSpace(jsonpeek.RootRawValue(data, "output"))
	if completion := responsecheck.JSONGeneration(data); completion != "completed" || len(output) == 0 || output[0] != '[' {
		return rejection, fmt.Errorf("账号检测未确认生成完成（%s）", completion)
	}
	if err := responsecheck.JSON(data); err != nil {
		return rejection, fmt.Errorf("账号检测响应无有效输出: %w", err)
	}
	return rejection, nil
}
