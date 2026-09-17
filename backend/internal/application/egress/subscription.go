package egress

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	maxSubscriptionEntries = 10000
	// 解码上限与传输层字节上限保持同一数量级(纯文本/base64 解析保护)。
	maxDecodedSubscriptionBytes = 2 << 20
)

type subscriptionEntry struct {
	ProxyURL string
	Key      string
}

func NormalizeSubscriptionURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxProxyURLBytes {
		return "", errors.New("订阅地址为空或过长")
	}
	if strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return "", errors.New("订阅地址包含控制字符")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("订阅地址格式无效")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("订阅地址必须使用 HTTP 或 HTTPS")
	}
	if parsed.Fragment != "" {
		return "", errors.New("订阅地址不能包含片段")
	}
	return parsed.String(), nil
}

// parseProxySubscription 解析订阅正文为可导入的节点条目。传输机制
// (HTTP 客户端、代理 scheme、重定向、SSRF 收窄、体积上限)归
// infra/egress 的 SubscriptionFetcher;本函数只做纯文本解析与代际记账。
func parseProxySubscription(value string) ([]subscriptionEntry, int, error) {
	entries, skipped := parseProxyLines(value)
	if len(entries) > 0 {
		return entries, skipped, nil
	}
	compact := strings.Map(func(character rune) rune {
		if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			return -1
		}
		return character
	}, strings.TrimPrefix(value, "\ufeff"))
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(compact)
		if err != nil || len(decoded) == 0 || len(decoded) > maxDecodedSubscriptionBytes {
			continue
		}
		entries, decodedSkipped := parseProxyLines(string(decoded))
		if len(entries) > 0 {
			// The original Base64 text is not an invalid proxy entry. Once it
			// decodes to a valid list, report only invalid decoded entries.
			return entries, decodedSkipped, nil
		}
	}
	if entries, clashSkipped, matched := parseClashSubscription(value); matched && len(entries) > 0 {
		return entries, clashSkipped, nil
	}
	return nil, skipped, errors.New("订阅中没有可用的代理节点")
}

func parseProxyLines(value string) ([]subscriptionEntry, int) {
	value = strings.TrimPrefix(value, "\ufeff")
	seen := make(map[string]struct{})
	entries := make([]subscriptionEntry, 0)
	skipped := 0
	for line := range strings.SplitSeq(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		normalized, err := NormalizeProxyURL(line)
		if err != nil {
			skipped++
			continue
		}
		digest := sha256.Sum256([]byte(normalized))
		key := hex.EncodeToString(digest[:])
		if _, exists := seen[key]; exists {
			skipped++
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, subscriptionEntry{ProxyURL: normalized, Key: key})
		if len(entries) > maxSubscriptionEntries {
			return nil, skipped
		}
	}
	return entries, skipped
}

func sourceNodeName(sourceName string, index int) string {
	suffix := fmt.Sprintf(" %03d", index+1)
	value := strings.TrimSpace(sourceName)
	for len(value)+len(suffix) > 160 && value != "" {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value) + suffix
}
