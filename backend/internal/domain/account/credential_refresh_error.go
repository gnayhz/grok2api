package account

import "strings"

// NormalizeCredentialRefreshErrorMessage sanitizes a diagnostic message stored
// on the credential after a refresh failure.
func NormalizeCredentialRefreshErrorMessage(value string) string {
	value = strings.Map(func(char rune) rune {
		switch char {
		case '\r', '\n', '\t':
			return ' '
		}
		if char < 0x20 || char == 0x7f {
			return -1
		}
		return char
	}, strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 512 {
		value = string(runes[:511]) + "…"
	}
	return value
}

// NormalizeCredentialRefreshErrorResponse sanitizes an upstream response body
// stored for operator diagnosis after a refresh failure.
func NormalizeCredentialRefreshErrorResponse(value string) string {
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, strings.TrimSpace(value))
	runes := []rune(value)
	if len(runes) > 4096 {
		value = string(runes[:4095]) + "…"
	}
	return value
}
