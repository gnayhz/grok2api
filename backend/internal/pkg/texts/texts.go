// Package texts 提供跨包共享的小型文本工具，避免相同实现 drift。
package texts

import "strings"

// FirstNonEmpty 返回第一个去除首尾空白后非空的值；命中的值保持原样返回（不裁剪空白）。
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// FirstNonEmptyTrimmed 返回第一个非全空白值裁剪首尾空白后的形式。
// 与 FirstNonEmpty 的区别仅在命中值是否保留原始空白。
func FirstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// TruncateRunes 按 rune 截断字符串；limit 非正数时返回空串。
func TruncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
