package jsonpeek

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// Valid validates the entire JSON frame before borrowed fields become evidence.
// Use the standard validator as the authority. Fast skip routines can accept
// truncated literals or incomplete strings and cannot establish this contract.
func Valid(data []byte) bool { return json.Valid(data) }

// ObjectFields visits the immediate fields of a complete JSON object. Values
// borrow data; callers must validate untrusted JSON before using them as
// evidence. Returning false stops iteration. Nested keys are never visited.
func ObjectFields(data []byte, visit func(key, value []byte) bool) {
	data = skipJSONSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return
	}
	data = skipJSONSpace(data[1:])
	for len(data) > 0 && data[0] == '"' {
		end := matchJSONString(data)
		if end < 2 {
			return
		}
		key := UnquoteBytes(data[:end])
		data = skipJSONSpace(data[end:])
		if len(data) == 0 || data[0] != ':' {
			return
		}
		data = skipJSONSpace(data[1:])
		end = scanJSONValue(data)
		if end <= 0 || !visit(key, data[:end]) {
			return
		}
		data = skipJSONSpace(data[end:])
		if len(data) == 0 || data[0] != ',' {
			return
		}
		data = skipJSONSpace(data[1:])
	}
}

// ArrayValues is the array counterpart of ObjectFields.
func ArrayValues(data []byte, visit func(value []byte) bool) {
	data = skipJSONSpace(data)
	if len(data) == 0 || data[0] != '[' {
		return
	}
	data = skipJSONSpace(data[1:])
	for len(data) > 0 && data[0] != ']' {
		end := scanJSONValue(data)
		if end <= 0 || !visit(data[:end]) {
			return
		}
		data = skipJSONSpace(data[end:])
		if len(data) == 0 || data[0] != ',' {
			return
		}
		data = skipJSONSpace(data[1:])
	}
}

// UnquoteBytes borrows unescaped strings and decodes JSON escapes otherwise.
// JSON strings differ from Go strings: escaped slashes and surrogate pairs
// must be accepted, so strconv.Unquote is not a substitute for the decoder.
func UnquoteBytes(raw []byte) []byte {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil
	}
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner) {
		return inner
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return []byte(value)
}

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
	var stack [34]byte
	needle := quotedKeyNeedle(&stack, key)
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

// RootStringBytes returns the root-object string value for key as a slice of
// data (no copy) when the value has no JSON escapes. Event "type" fields
// never escape; callers that need a string should intern known values
// instead of string()-converting every frame.
func RootStringBytes(data []byte, key string) []byte {
	if len(data) == 0 || key == "" {
		return nil
	}
	data = skipJSONSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil
	}
	data = skipJSONSpace(data[1:])
	for len(data) > 0 {
		data = skipJSONSpace(data)
		if len(data) == 0 || data[0] == '}' {
			return nil
		}
		if data[0] != '"' {
			return nil
		}
		end := matchJSONString(data)
		if end <= 1 {
			return nil
		}
		field := data[1 : end-1]
		data = skipJSONSpace(data[end:])
		if len(data) == 0 || data[0] != ':' {
			return nil
		}
		data = skipJSONSpace(data[1:])
		value := extractJSONValue(data)
		if len(value) == 0 {
			return nil
		}
		if bytesEqualString(field, key) && value[0] == '"' {
			inner := matchJSONString(value)
			if inner > 1 {
				return value[1 : inner-1]
			}
			return nil
		}
		data = skipJSONSpace(data[len(value):])
		if len(data) > 0 && data[0] == ',' {
			data = data[1:]
		}
	}
	return nil
}

// RootStringField returns the string value for key on the root object only.
// StringField searches the whole buffer and can hit nested keys first after
// encoding/json rewrites (map keys sorted, "response" before "type").
func RootStringField(data []byte, key string) string {
	return string(RootStringBytes(data, key))
}

// internedTypes are SSE event types on the quality-scan and client-delivery
// hot paths. InternType returns these strings without allocating.
var internedTypes = [...]struct {
	b []byte
	s string
}{
	{[]byte("response.created"), "response.created"},
	{[]byte("response.in_progress"), "response.in_progress"},
	{[]byte("response.reasoning_summary_part.added"), "response.reasoning_summary_part.added"},
	{[]byte("response.content_part.added"), "response.content_part.added"},
	{[]byte("response.reasoning_text.delta"), "response.reasoning_text.delta"},
	{[]byte("response.reasoning_summary_text.delta"), "response.reasoning_summary_text.delta"},
	{[]byte("response.output_text.delta"), "response.output_text.delta"},
	{[]byte("response.refusal.delta"), "response.refusal.delta"},
	{[]byte("response.function_call_arguments.delta"), "response.function_call_arguments.delta"},
	{[]byte("response.custom_tool_call_input.delta"), "response.custom_tool_call_input.delta"},
	{[]byte("response.mcp_call_arguments.delta"), "response.mcp_call_arguments.delta"},
	{[]byte("response.output_item.added"), "response.output_item.added"},
	{[]byte("response.output_item.done"), "response.output_item.done"},
	{[]byte("response.completed"), "response.completed"},
	{[]byte("response.failed"), "response.failed"},
	{[]byte("response.incomplete"), "response.incomplete"},
	{[]byte("response.error"), "response.error"},
	{[]byte("error"), "error"},
	{[]byte("message_stop"), "message_stop"},
	{[]byte("ping"), "ping"},
	{[]byte("content_block_delta"), "content_block_delta"},
	{[]byte("image_generation.completed"), "image_generation.completed"},
	{[]byte("image_generation.failed"), "image_generation.failed"},
}

// InternType maps known SSE type bytes to interned strings. Unknown values
// still allocate. Event types never contain JSON escapes.
func InternType(b []byte) string {
	for i := range internedTypes {
		if bytes.Equal(b, internedTypes[i].b) {
			return internedTypes[i].s
		}
	}
	if len(b) == 0 {
		return ""
	}
	return string(b)
}

// RootStringFieldScan visits root fields of a complete JSON object. Whitespace,
// escaped keys and duplicate fields follow JSON decoding semantics (last wins).
// Unescaped values borrow input until the final string conversion.
func RootStringFieldScan(data []byte, key string) string {
	if len(data) == 0 || key == "" {
		return ""
	}
	var found []byte
	ObjectFields(data, func(field, value []byte) bool {
		if bytesEqualString(field, key) {
			found = UnquoteBytes(value)
		}
		return true
	})
	return string(found)
}

// RootIntFieldScan returns the last root integer for key. It does not validate
// the whole frame; callers using it as evidence must first validate the input.
func RootIntFieldScan(data []byte, key string) (int64, bool) {
	if len(data) == 0 || key == "" {
		return 0, false
	}
	var found int64
	var valid bool
	ObjectFields(data, func(field, raw []byte) bool {
		if bytesEqualString(field, key) {
			value, err := strconv.ParseInt(string(raw), 10, 64)
			found, valid = value, err == nil
		}
		return true
	})
	return found, valid
}

// RootRawValue returns the last immediate field value, borrowing data. As with
// ObjectFields, callers establish complete valid JSON before trusting it.
func RootRawValue(data []byte, key string) []byte {
	var found []byte
	ObjectFields(data, func(field, value []byte) bool {
		if bytesEqualString(field, key) {
			found = value
		}
		return true
	})
	return found
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

// bytesEqualString 比较字节切片与字符串而无分配——根层键遍历对每个
// 非目标键都要比较一次，string(field) 转换曾是 jsonpeek 的分配大头
// （检查器流基准 ~98% 的分配来自 RootStringField 的逐键转换）。
func bytesEqualString(field []byte, key string) bool {
	if len(field) != len(key) {
		return false
	}
	for i := 0; i < len(key); i++ {
		if field[i] != key[i] {
			return false
		}
	}
	return true
}

// quotedKeyNeedle builds a quoted JSON object key for bytes.Index. stack
// holds keys up to 32 bytes so the scanner hot path does not heap-allocate
// a needle per frame.
func quotedKeyNeedle(stack *[34]byte, key string) []byte {
	need := len(key) + 2
	var needle []byte
	if need > len(stack) {
		needle = make([]byte, 0, need)
	} else {
		needle = stack[:0]
	}
	needle = append(needle, '"')
	needle = append(needle, key...)
	needle = append(needle, '"')
	return needle
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
	var stack [34]byte
	return bytes.Contains(data, quotedKeyNeedle(&stack, key))
}

// IntField returns the first JSON numeric value for key. String values for the
// same key are skipped. Used to pull usage off the tail of a huge completed
// event without scanning the encrypted_content body.
func IntField(data []byte, key string) (int64, bool) {
	if len(data) == 0 || key == "" {
		return 0, false
	}
	var stack [34]byte
	needle := quotedKeyNeedle(&stack, key)
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

// TokenUsageObject reads only counters belonging to one complete usage object.
// Unlike the fragment-compatible TokenUsageFrom, it never searches for a
// nested usage field. Canonical protocol observers should use this boundary.
func TokenUsageObject(data []byte) TokenUsage {
	if !Valid(data) {
		return TokenUsage{}
	}
	return tokenUsageObject(data)
}

// TokenUsageFrom reads usage counters from a JSON fragment, preferring a
// nested "usage" object when present so ciphertext in the same buffer cannot
// supply the first numeric match.
//
// Keys are case-sensitive (unlike encoding/json struct tags). Snake_case is
// the production contract; camelCase inputTokens/outputTokens/totalTokens are
// kept as fallbacks to match the old inspector DTO.
func TokenUsageFrom(data []byte) TokenUsage {
	if raw := RawValue(data, "usage"); len(raw) > 0 && raw[0] == '{' && Valid(raw) {
		return tokenUsageObject(raw)
	}
	if Valid(data) {
		return tokenUsageObject(data)
	}
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

// UnquotedBytes returns the first JSON string value for key after decoding
// JSON escapes, without allocating when the value has no backslash escapes
// (the common SSE delta case). The returned slice aliases data in that
// case and must not be retained across a later write to the same buffer.
func UnquotedBytes(data []byte, key string) []byte {
	raw := RawValue(data, key)
	if len(raw) < 2 || raw[0] != '"' {
		return nil
	}
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return inner
	}
	s, err := strconv.Unquote(string(raw))
	if err != nil {
		return nil
	}
	return []byte(s)
}

// UnquotedStringField returns the first JSON string value for key after
// decoding JSON escapes. StringField returns the raw inner bytes, so a
// payload of "\n" would look like two visible runes instead of a newline.
func UnquotedStringField(data []byte, key string) string {
	return string(UnquotedBytes(data, key))
}

// RawValue returns the raw JSON value for the first object key, including
// truncated buffers as long as that value itself is complete. Brace matching
// ignores braces inside strings so a cut-off encrypted_content suffix does not
// prevent extracting a leading "error" object.
func RawValue(data []byte, key string) []byte {
	if len(data) == 0 || key == "" {
		return nil
	}
	var stack [34]byte
	needle := quotedKeyNeedle(&stack, key)
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
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch c {
		case '"':
			end := matchJSONString(data[i:])
			if end < 0 {
				return -1
			}
			i += end - 1
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
	for start := 1; start < len(data); {
		end := bytes.IndexByte(data[start:], '"')
		if end < 0 {
			return -1
		}
		end += start
		// Only an odd run of backslashes escapes a quote. Skip long strings
		// (notably ciphertext) with the optimized byte search, not a byte loop.
		slashes := 0
		for i := end - 1; i > 0 && data[i] == '\\'; i-- {
			slashes++
		}
		if slashes%2 == 0 {
			return end + 1
		}
		start = end + 1
	}
	return -1
}

// tokenUsageObject keeps top-level usage separate from context and detail
// counters. A nested context_details.input_tokens must never shadow prompt_tokens.
func tokenUsageObject(data []byte) TokenUsage {
	var usage TokenUsage
	read := func(source []byte, keys ...string) int64 {
		for _, key := range keys {
			if value, ok := RootIntFieldScan(source, key); ok {
				usage.Found = true
				return value
			}
		}
		return 0
	}
	usage.Input = read(data, "input_tokens", "prompt_tokens", "inputTokens")
	usage.Output = read(data, "output_tokens", "completion_tokens", "outputTokens")
	usage.Total = read(data, "total_tokens", "totalTokens")
	usage.Reasoning = read(data, "reasoning_tokens", "thinking_tokens")
	if usage.Reasoning == 0 {
		for _, key := range []string{"output_tokens_details", "completion_tokens_details"} {
			if raw := RootRawValue(data, key); len(raw) > 0 {
				usage.Reasoning = read(raw, "reasoning_tokens", "thinking_tokens")
				break
			}
		}
	}
	usage.Cached = read(data, "cache_read_input_tokens", "cached_tokens")
	if usage.Cached == 0 {
		for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
			if raw := RootRawValue(data, key); len(raw) > 0 {
				usage.Cached = read(raw, "cached_tokens")
				break
			}
		}
	}
	usage.CacheCreation = read(data, "cache_creation_input_tokens")
	usage.CostTicks = read(data, "cost_in_usd_ticks")
	usage.Sources = read(data, "num_sources_used")
	usage.ServerTools = read(data, "num_server_side_tools_used")
	// Context detail presence alone does not establish billable token usage.
	if raw := RootRawValue(data, "context_details"); len(raw) > 0 {
		usage.ContextInput, _ = RootIntFieldScan(raw, "input_tokens")
		usage.ContextOutput, _ = RootIntFieldScan(raw, "output_tokens")
	}
	return usage
}
