// Package proxyurl 提供代理地址归一化的唯一可信实现。
// 历史上该函数位于 application/egress，导致 infra/egress 为复用它而
// 反向导入应用层（职责越界）。移至中立 pkg 后，应用层与基础设施层都
// 依赖 pkg，依赖方向恢复为严格向下（同 pkg/cfcookies 的先例）。
package proxyurl

import (
	"errors"
	"net/url"
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

const (
	maxProxyURLBytes     = 8192
	proxyAccountSentinel = "grok2api_account_placeholder"
)

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

// NormalizeProxyURL 校验并归一化代理地址；详见各错误分支的语义注释。
func NormalizeProxyURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maxProxyURLBytes || strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return "", errors.New("代理地址过长或包含控制字符")
	}
	// 非 ASCII 主机在 url.String() 时被百分号编码膨胀(每字节×3):接近上限的
	// 输入一次归一化通过、其输出再归一化被拒(非幂等), 且编码后主机是死地址。
	// 先拒绝包含任何非 ASCII 字节的输入, 保证输出恒为纯 ASCII = 幂等。
	if !isASCII(value) {
		return "", errors.New("代理地址只能包含 ASCII 字符")
	}
	hasAccountPlaceholder := domain.IsAccountTemplateProxy(value)
	if strings.Count(value, domain.ProxyAccountPlaceholder) > 1 {
		return "", errors.New("代理地址最多包含一个 {account} 占位符")
	}
	if hasAccountPlaceholder && strings.Contains(value, proxyAccountSentinel) {
		return "", errors.New("代理地址包含保留的账号占位符文本")
	}
	parseValue := strings.ReplaceAll(value, domain.ProxyAccountPlaceholder, proxyAccountSentinel)
	parsed, err := url.Parse(parseValue)
	if err != nil {
		return "", errors.New("代理地址格式无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if tunnelproxy.IsSupportedScheme(scheme) {
		if hasAccountPlaceholder {
			return "", errors.New("隧道代理不支持 {account} 占位符")
		}
		normalized, normalizeErr := tunnelproxy.Normalize(value)
		if normalizeErr != nil {
			return "", normalizeErr
		}
		return normalized, nil
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", errors.New("代理地址格式无效")
	}
	switch scheme {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return "", errors.New("代理地址协议必须是 HTTP、HTTPS、SOCKS4、SOCKS5、Trojan、VLESS、SS 或 VMess")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("代理地址不能包含路径、查询参数或片段")
	}
	if hasAccountPlaceholder {
		if parsed.User == nil || !strings.Contains(parsed.User.Username(), proxyAccountSentinel) {
			return "", errors.New("{account} 只能用于代理认证用户名")
		}
		return strings.ReplaceAll(parsed.String(), proxyAccountSentinel, domain.ProxyAccountPlaceholder), nil
	}
	return parsed.String(), nil
}
