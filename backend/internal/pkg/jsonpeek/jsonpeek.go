package jsonpeek

import (
	"bytes"
	"strconv"
)

// StringField returns the first JSON string value for key without decoding the
// rest of the document. It is for hot-path SSE frames where encoding/json would
// otherwise scan a multi-MiB encrypted_content field we do not need.
//
// Only the first matching object key is returned. Nested objects are not
// walked: callers that need a nested field must pass a prefix that contains it
// (typical Responses events put type/id in the first few hundred bytes).
func StringField(data []byte, key string) string {
	if len(data) == 0 || key == "" {
		return ""
	}
	needle := make([]byte, 0, len(key)+2)
	needle = append(needle, '"')
	needle = append(needle, key...)
	needle = append(needle, '"')
	start := 0
	for {
		index := bytes.Index(data[start:], needle)
		if index < 0 {
			return ""
		}
		index += start
		rest := data[index+len(needle):]
		rest = skipJSONSpace(rest)
		if len(rest) == 0 || rest[0] != ':' {
			start = index + 1
			continue
		}
		rest = skipJSONSpace(rest[1:])
		if len(rest) == 0 || rest[0] != '"' {
			start = index + 1
			continue
		}
		rest = rest[1:]
		end := 0
		for end < len(rest) {
			if rest[end] == 92 {
				end += 2
				continue
			}
			if rest[end] == '"' {
				return string(rest[:end])
			}
			end++
		}
		return ""
	}
}

// RootStringField returns the string value for key on the root object only.
// StringField searches the whole buffer and can hit nested keys first after
// encoding/json rewrites (map keys sorted, "response" before "type").
func RootStringField(data []byte, key string) string {
	if len(data) == 0 || key == "" {
		return ""
	}
	data = skipJSONSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return ""
	}
	data = skipJSONSpace(data[1:])
	for len(data) > 0 {
		data = skipJSONSpace(data)
		if len(data) == 0 || data[0] == '}' {
			return ""
		}
		if data[0] != '"' {
			return ""
		}
		end := matchJSONString(data)
		if end <= 1 {
			return ""
		}
		field := data[1 : end-1]
		data = skipJSONSpace(data[end:])
		if len(data) == 0 || data[0] != ':' {
			return ""
		}
		data = skipJSONSpace(data[1:])
		value := extractJSONValue(data)
		if len(value) == 0 {
			return ""
		}
		if string(field) == key && value[0] == '"' {
			inner := matchJSONString(value)
			if inner > 1 {
				return string(value[1 : inner-1])
			}
			return ""
		}
		data = skipJSONSpace(data[len(value):])
		if len(data) > 0 && data[0] == ',' {
			data = data[1:]
		}
	}
	return ""
}

// RootStringFieldScan returns the root-level string value for key on a
// COMPLETE JSON object of any size, walking past nested values of arbitrary
// length without allocating. Unlike RootStringField it is key-order
// independent on frames larger than any head window: callers that re-marshal
// through map[string]any get alphabetically sorted keys, so a multi-KB
// "response" object can precede "type" at the root. Returns "" when the
// buffer truncates before the key's value completes.
func RootStringFieldScan(data []byte, key string) string {
	if len(data) == 0 || key == "" {
		return ""
	}
	rest := skipJSONSpace(data)
	if len(rest) == 0 || rest[0] != '{' {
		return ""
	}
	rest = skipJSONSpace(rest[1:])
	for len(rest) > 0 {
		if rest[0] == '}' {
			return ""
		}
		if rest[0] != '"' {
			return ""
		}
		end := matchJSONString(rest)
		if end <= 1 {
			return ""
		}
		field := rest[1 : end-1]
		rest = skipJSONSpace(rest[end:])
		if len(rest) == 0 || rest[0] != ':' {
			return ""
		}
		rest = skipJSONSpace(rest[1:])
		valueEnd := scanJSONValue(rest)
		if valueEnd <= 0 {
			// Truncated value: the target key, when present, sits past the
			// buffer end. Callers wanting head-only semantics pass a prefix.
			return ""
		}
		if string(field) == key && rest[0] == '"' {
			return string(rest[1 : valueEnd-1])
		}
		rest = skipJSONSpace(rest[valueEnd:])
		if len(rest) > 0 && rest[0] == ',' {
			rest = rest[1:]
		}
	}
	return ""
}

// RootIntFieldScan returns the root-level integer value for key on a
// COMPLETE JSON object of any size, with the same key-order independence
// as RootStringFieldScan. Returns false when the key is absent, its value
// is not an integer literal, or the buffer truncates before it completes.
// 用于重排键序大帧上位于嵌套大对象之后的根层数值（如 sequence_number）。
func RootIntFieldScan(data []byte, key string) (int64, bool) {
	if len(data) == 0 || key == "" {
		return 0, false
	}
	rest := skipJSONSpace(data)
	if len(rest) == 0 || rest[0] != '{' {
		return 0, false
	}
	rest = skipJSONSpace(rest[1:])
	for len(rest) > 0 {
		if rest[0] == '}' {
			return 0, false
		}
		if rest[0] != '"' {
			return 0, false
		}
		end := matchJSONString(rest)
		if end <= 1 {
			return 0, false
		}
		field := rest[1 : end-1]
		rest = skipJSONSpace(rest[end:])
		if len(rest) == 0 || rest[0] != ':' {
			return 0, false
		}
		rest = skipJSONSpace(rest[1:])
		valueEnd := scanJSONValue(rest)
		if valueEnd <= 0 {
			return 0, false
		}
		if string(field) == key && rest[0] != '"' && rest[0] != '{' && rest[0] != '[' {
			value, err := strconv.ParseInt(string(rest[:valueEnd]), 10, 64)
			if err != nil {
				// 布尔/null/浮点字面量：该键不是整数，与缺失同口径。
				return 0, false
			}
			return value, true
		}
		rest = skipJSONSpace(rest[valueEnd:])
		if len(rest) > 0 && rest[0] == ',' {
			rest = rest[1:]
		}
	}
	return 0, false
}

// scanJSONValue returns the exclusive end offset of the JSON value at the
// start of data, or -1 when data truncates inside it (unterminated string or
// unbalanced brackets). Literal scalars end at the first value delimiter.
func scanJSONValue(data []byte) int {
	if len(data) == 0 {
		return -1
	}
	switch data[0] {
	case '"':
		return matchJSONString(data)
	case '{', '[':
		return matchJSONBrackets(data)
	default:
		end := 0
		for end < len(data) {
			c := data[end]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				break
			}
			end++
		}
		if end == 0 {
			return -1
		}
		return end
	}
}

func skipJSONSpace(data []byte) []byte {
	for len(data) > 0 {
		switch data[0] {
		case 32, 9, 10, 13:
			data = data[1:]
		default:
			return data
		}
	}
	return data
}

func Prefix(data []byte, n int) []byte {
	if n <= 0 || len(data) <= n {
		return data
	}
	return data[:n]
}

func Suffix(data []byte, n int) []byte {
	if n <= 0 || len(data) <= n {
		return data
	}
	return data[len(data)-n:]
}

func HasKey(data []byte, key string) bool {
	if len(data) == 0 || key == "" {
		return false
	}
	needle := make([]byte, 0, len(key)+2)
	needle = append(needle, '"')
	needle = append(needle, key...)
	needle = append(needle, '"')
	return bytes.Contains(data, needle)
}

// IntField returns the first JSON numeric value for key. String values for the
// same key are skipped. Used to pull usage off the tail of a huge completed
// event without scanning the encrypted_content body.
func IntField(data []byte, key string) (int64, bool) {
	if len(data) == 0 || key == "" {
		return 0, false
	}
	needle := make([]byte, 0, len(key)+2)
	needle = append(needle, '"')
	needle = append(needle, key...)
	needle = append(needle, '"')
	start := 0
	for {
		index := bytes.Index(data[start:], needle)
		if index < 0 {
			return 0, false
		}
		index += start
		rest := skipJSONSpace(data[index+len(needle):])
		if len(rest) == 0 || rest[0] != ':' {
			start = index + 1
			continue
		}
		rest = skipJSONSpace(rest[1:])
		if len(rest) == 0 || rest[0] == '"' {
			start = index + 1
			continue
		}
		end := 0
		if rest[0] == '-' {
			end = 1
		}
		if end >= len(rest) || rest[end] < '0' || rest[end] > '9' {
			start = index + 1
			continue
		}
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		n, err := strconv.ParseInt(string(rest[:end]), 10, 64)
		if err != nil {
			start = index + 1
			continue
		}
		return n, true
	}
}

type TokenUsage struct {
	Input         int64
	Output        int64
	Total         int64
	Reasoning     int64
	Cached        int64
	CacheCreation int64
	CostTicks     int64
	Sources       int64
	ServerTools   int64
	ContextInput  int64
	ContextOutput int64
	Found         bool
}

// TokenUsageFrom reads usage counters from a JSON fragment, preferring a
// nested "usage" object when present so ciphertext in the same buffer cannot
// supply the first numeric match.
//
// Keys are case-sensitive (unlike encoding/json struct tags). Snake_case is
// the production contract; camelCase inputTokens/outputTokens/totalTokens are
// kept as fallbacks to match the old inspector DTO.
func TokenUsageFrom(data []byte) TokenUsage {
	if idx := bytes.Index(data, []byte(`"usage"`)); idx >= 0 {
		data = data[idx:]
	}
	var usage TokenUsage
	if v, ok := IntField(data, "output_tokens"); ok {
		usage.Output = v
		usage.Found = true
	} else if v, ok := IntField(data, "completion_tokens"); ok {
		usage.Output = v
		usage.Found = true
	} else if v, ok := IntField(data, "outputTokens"); ok {
		usage.Output = v
		usage.Found = true
	}
	if v, ok := IntField(data, "input_tokens"); ok {
		usage.Input = v
		usage.Found = true
	} else if v, ok := IntField(data, "prompt_tokens"); ok {
		usage.Input = v
		usage.Found = true
	} else if v, ok := IntField(data, "inputTokens"); ok {
		usage.Input = v
		usage.Found = true
	}
	if v, ok := IntField(data, "total_tokens"); ok {
		usage.Total = v
		usage.Found = true
	} else if v, ok := IntField(data, "totalTokens"); ok {
		usage.Total = v
		usage.Found = true
	}
	if v, ok := IntField(data, "reasoning_tokens"); ok {
		usage.Reasoning = v
		usage.Found = true
	} else if v, ok := IntField(data, "thinking_tokens"); ok {
		usage.Reasoning = v
		usage.Found = true
	}
	if v, ok := IntField(data, "cached_tokens"); ok {
		usage.Cached = v
		usage.Found = true
	} else if v, ok := IntField(data, "cache_read_input_tokens"); ok {
		usage.Cached = v
		usage.Found = true
	}
	if v, ok := IntField(data, "cache_creation_input_tokens"); ok {
		usage.CacheCreation = v
	}
	if v, ok := IntField(data, "cost_in_usd_ticks"); ok {
		usage.CostTicks = v
	}
	if v, ok := IntField(data, "num_sources_used"); ok {
		usage.Sources = v
	}
	if v, ok := IntField(data, "num_server_side_tools_used"); ok {
		usage.ServerTools = v
	}
	if idx := bytes.Index(data, []byte(`"context_details"`)); idx >= 0 {
		ctx := data[idx:]
		if v, ok := IntField(ctx, "input_tokens"); ok {
			usage.ContextInput = v
		}
		if v, ok := IntField(ctx, "output_tokens"); ok {
			usage.ContextOutput = v
		}
	}
	return usage
}

// RawValue returns the raw JSON value for the first object key, including
// truncated buffers as long as that value itself is complete. Brace matching
// ignores braces inside strings so a cut-off encrypted_content suffix does not
// prevent extracting a leading "error" object.
func RawValue(data []byte, key string) []byte {
	if len(data) == 0 || key == "" {
		return nil
	}
	needle := make([]byte, 0, len(key)+2)
	needle = append(needle, '"')
	needle = append(needle, key...)
	needle = append(needle, '"')
	start := 0
	for {
		index := bytes.Index(data[start:], needle)
		if index < 0 {
			return nil
		}
		index += start
		rest := skipJSONSpace(data[index+len(needle):])
		if len(rest) == 0 || rest[0] != ':' {
			start = index + 1
			continue
		}
		rest = skipJSONSpace(rest[1:])
		return extractJSONValue(rest)
	}
}

func extractJSONValue(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	switch data[0] {
	case '{', '[':
		end := matchJSONBrackets(data)
		if end <= 0 {
			return nil
		}
		return data[:end]
	case '"':
		end := matchJSONString(data)
		if end <= 0 {
			return nil
		}
		return data[:end]
	case 'n':
		if bytes.HasPrefix(data, []byte("null")) {
			return data[:4]
		}
	case 't':
		if bytes.HasPrefix(data, []byte("true")) {
			return data[:4]
		}
	case 'f':
		if bytes.HasPrefix(data, []byte("false")) {
			return data[:5]
		}
	default:
		end := 0
		if data[0] == '-' {
			end = 1
		}
		if end >= len(data) || data[end] < '0' || data[end] > '9' {
			return nil
		}
		for end < len(data) && data[end] >= '0' && data[end] <= '9' {
			end++
		}
		return data[:end]
	}
	return nil
}

func matchJSONBrackets(data []byte) int {
	depth := 0
	inString := false
	escape := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			if c == 92 {
				escape = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1
			}
			if depth < 0 {
				return -1
			}
		}
	}
	return -1
}

func matchJSONString(data []byte) int {
	if len(data) == 0 || data[0] != '"' {
		return -1
	}
	escape := false
	for i := 1; i < len(data); i++ {
		if escape {
			escape = false
			continue
		}
		if data[i] == 92 {
			escape = true
			continue
		}
		if data[i] == '"' {
			return i + 1
		}
	}
	return -1
}
