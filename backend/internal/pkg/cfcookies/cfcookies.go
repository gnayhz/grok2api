// Package cfcookies 提供_cloudflare 会话 Cookie 的纯字符串净化:白名单
// 名称(cf_clearance/__cf_bm/_cfuvid/cf_chl_*)、去重、长度与控制字符约束。
// 该能力被出口层(Clearance 生命周期)与账号层(凭据入库)共同使用, 放在
// 中立 pkg 是为了让账号层不必为此导入出口应用包(业务与代理解耦)。
package cfcookies

import "strings"

const maxCloudflareCookieBytes = 16 << 10

// Sanitize 返回仅含允许的 Cloudflare 会话 Cookie 的分号串联串; 无有效项
// 时返回空串。
func Sanitize(value string) string {
	allowed := make([]string, 0, 4)
	seen := make(map[string]struct{})
	for part := range strings.SplitSeq(value, ";") {
		name, cookieValue, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		lower := strings.ToLower(name)
		if lower != "cf_clearance" && lower != "__cf_bm" && lower != "_cfuvid" && !strings.HasPrefix(lower, "cf_chl_") {
			continue
		}
		if _, exists := seen[lower]; exists {
			continue
		}
		cookieValue = strings.TrimSpace(cookieValue)
		if cookieValue == "" || len(cookieValue) > maxCloudflareCookieBytes || strings.IndexFunc(cookieValue, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
			continue
		}
		seen[lower] = struct{}{}
		allowed = append(allowed, lower+"="+cookieValue)
	}
	return strings.Join(allowed, "; ")
}
