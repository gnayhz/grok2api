package clientkey

import "strings"

// clientKeyScheme 是客户端 Key 的稳定前缀；格式规则归本领域，
// 加解密与随机源能力由外部注入。
const clientKeyScheme = "g2a"

// FormatClientKey 生成 g2a_<prefix>_<secret> 格式的客户端 Key。
func FormatClientKey(prefix, secret string) string {
	return clientKeyScheme + "_" + prefix + "_" + secret
}

// SplitClientKey 解析 g2a_<prefix>_<secret> 格式的客户端 Key，返回 prefix。
func SplitClientKey(raw string) (string, bool) {
	parts := strings.SplitN(raw, "_", 3)
	if len(parts) != 3 || parts[0] != clientKeyScheme || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return parts[1], true
}
