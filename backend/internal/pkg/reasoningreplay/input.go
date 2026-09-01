package reasoningreplay

import (
	"encoding/json"
	"strings"
)

// replayTurn 是单轮响应的可回放输出(reasoning + 工具调用/消息),注入时
// 作为整体寻找锚点。上游提示缓存按严格前缀扩展复用:注入位置跨轮漂移或
// 上一轮注入的内容下一轮消失,都会让前缀在早期分叉,缓存收益塌缩到公共
// 头部(线上实测 cached 约 128 token)。因此本文件的注入规则只有一条硬约束:
// 同一会话相邻两轮的上游序列必须满足「上一轮是本轮的纯前缀」。
type replayTurn struct {
	items [][]byte
}

// inputReplayIndex 预计算请求输入中与回放去重相关的索引:已存在的密文、
// 工具调用与输出映射、assistant 消息内容签名。
type inputReplayIndex struct {
	existingCalls    map[string]bool
	existingOutputs  map[string]string
	existingEncypted map[string]bool
	assistantTexts   map[string]struct{}
}

func buildInputReplayIndex(inputItems []map[string]json.RawMessage) inputReplayIndex {
	index := inputReplayIndex{
		existingCalls:    map[string]bool{},
		existingOutputs:  map[string]string{},
		existingEncypted: map[string]bool{},
		assistantTexts:   map[string]struct{}{},
	}
	for _, item := range inputItems {
		var typeName string
		_ = json.Unmarshal(item["type"], &typeName)
		typeName = strings.TrimSpace(typeName)
		switch typeName {
		case "reasoning":
			var enc string
			_ = json.Unmarshal(item["encrypted_content"], &enc)
			if enc != "" {
				index.existingEncypted[enc] = true
			}
		case "function_call_output", "custom_tool_call_output":
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			for _, candidate := range comparableReplayCallIDs(callID) {
				index.existingOutputs[candidate] = callID
			}
		case "function_call", "custom_tool_call":
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			for _, key := range replayToolCallKeys(typeName, callID) {
				index.existingCalls[key] = true
			}
		case "message", "":
			var role string
			_ = json.Unmarshal(item["role"], &role)
			if strings.EqualFold(strings.TrimSpace(role), "assistant") {
				if signature := assistantSignature(item); signature != "" {
					index.assistantTexts[signature] = struct{}{}
				}
			}
		}
	}
	return index
}

// filterTurnItems 保留该轮中尚未出现在请求输入里的项:输入已有的密文与
// 工具调用跳过;assistant 消息在客户端已重发(同文本存在于输入)时跳过。
func (t replayTurn) filterItems(index inputReplayIndex) [][]byte {
	filtered := make([][]byte, 0, len(t.items))
	for _, item := range t.items {
		var typed struct {
			Type             string "json:\"type\""
			Role             string "json:\"role\""
			EncryptedContent string "json:\"encrypted_content\""
			CallID           string "json:\"call_id\""
		}
		if json.Unmarshal(item, &typed) != nil {
			continue
		}
		switch strings.TrimSpace(typed.Type) {
		case "reasoning":
			if index.existingEncypted[typed.EncryptedContent] {
				continue
			}
		case "message":
			if strings.EqualFold(strings.TrimSpace(typed.Role), "assistant") {
				if _, resent := index.assistantTexts[assistantSignatureBytes(item)]; resent {
					continue
				}
			}
		case "function_call", "custom_tool_call":
			keys := replayToolCallKeys(strings.TrimSpace(typed.Type), typed.CallID)
			if len(keys) == 0 || anyReplayCallKeyExists(index.existingCalls, keys) {
				continue
			}
			outputCallID := ""
			for _, candidate := range comparableReplayCallIDs(typed.CallID) {
				if value := index.existingOutputs[candidate]; value != "" {
					outputCallID = value
					break
				}
			}
			if outputCallID == "" {
				continue
			}
			for _, key := range keys {
				index.existingCalls[key] = true
			}
			if outputCallID != typed.CallID {
				item = rewriteReplayCallID(item, outputCallID)
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// groupReplayTurns 把回放项按轮次分组:每个 reasoning 项开启新的一轮,
// 其后的工具调用/消息跟随该轮。没有 reasoning 前导的散项(理论边界)自成
// 一组,锚点找不到时整组跳过,不会污染请求前缀。
func groupReplayTurns(items [][]byte) []replayTurn {
	turns := make([]replayTurn, 0, 2)
	for _, raw := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		var typeName string
		_ = json.Unmarshal(item["type"], &typeName)
		if strings.TrimSpace(typeName) == "reasoning" || len(turns) == 0 {
			turns = append(turns, replayTurn{})
		}
		turns[len(turns)-1].items = append(turns[len(turns)-1].items, raw)
	}
	return turns
}

// anchorIndex 计算该轮在输入中的插入位置(返回 -1 表示找不到锚点)。
// 锚点优先级:
//  1. 输入中与该轮工具调用 call_id 匹配的 function_call_output 之前——工具
//     循环轮的 reasoning 必须紧邻它自己的调用簇,插到别处会破坏前缀;
//  2. 输入中与该轮工具调用 call_id 匹配的 function_call 之前——客户端重发
//     了调用但剥掉了 reasoning 的形态;
//  3. 输入中最后一条与该轮 assistant 文本一致的 assistant 消息之前——
//     纯文本轮的稳定追加位。
//
// 没有任何锚点时返回 -1:宁可放弃这一轮的回放,也不把外轮内容插进请求
// 头部。历史回归:无锚点注入曾把上一会话的推理塞进新对话的最前面,上游
// 前缀在第一个缓存块后分叉,大会话每轮全量重算。
func (t replayTurn) anchorIndex(input []map[string]json.RawMessage) int {
	callIDs := map[string]bool{}
	for _, raw := range t.items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		var typeName, callID string
		_ = json.Unmarshal(item["type"], &typeName)
		_ = json.Unmarshal(item["call_id"], &callID)
		typeName = strings.TrimSpace(typeName)
		if typeName != "function_call" && typeName != "custom_tool_call" {
			continue
		}
		for _, id := range comparableReplayCallIDs(callID) {
			callIDs[id] = true
		}
	}
	if len(callIDs) > 0 {
		// 客户端已重发 function_call 时,reasoning 锚在它自己的调用之前;
		// 只有调用未被重发(仅剩输出)时,才把 [reasoning, call] 一并插到
		// 匹配输出之前。
		for index, item := range input {
			if toolCallAnchor(item, callIDs, false) {
				return index
			}
		}
		for index, item := range input {
			if toolCallAnchor(item, callIDs, true) {
				return index
			}
		}
	}
	for _, raw := range t.items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		var typeName, role string
		_ = json.Unmarshal(item["type"], &typeName)
		_ = json.Unmarshal(item["role"], &role)
		if strings.TrimSpace(typeName) != "message" || !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}
		for index := len(input) - 1; index >= 0; index-- {
			inputType, inputRole := "", ""
			_ = json.Unmarshal(input[index]["type"], &inputType)
			_ = json.Unmarshal(input[index]["role"], &inputRole)
			if (strings.TrimSpace(inputType) == "" || strings.TrimSpace(inputType) == "message") && strings.EqualFold(strings.TrimSpace(inputRole), "assistant") && assistantContentEqual(input[index], item) {
				return index
			}
		}
	}
	return -1
}

// toolCallAnchor 判断输入项是否是与回放 call_id 匹配的工具调用(或其输出)。
func toolCallAnchor(item map[string]json.RawMessage, callIDs map[string]bool, output bool) bool {
	var typeName, callID string
	_ = json.Unmarshal(item["type"], &typeName)
	_ = json.Unmarshal(item["call_id"], &callID)
	typeName = strings.TrimSpace(typeName)
	if output {
		if typeName != "function_call_output" && typeName != "custom_tool_call_output" {
			return false
		}
	} else if typeName != "function_call" && typeName != "custom_tool_call" {
		return false
	}
	for _, candidate := range comparableReplayCallIDs(callID) {
		if callIDs[candidate] {
			return true
		}
	}
	return false
}

// insertReplayItems 将回放项按轮次锚定注入 Responses body.input。
// 每一轮先用完整轮内容寻锚(锚点信息不能先被过滤掉),锚定后再做轮内
// 去重;未命中锚点的轮次整组跳过。
func insertReplayItems(body []byte, replayItems [][]byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, false
	}
	inputRaw, ok := payload["input"]
	if !ok {
		return body, false
	}
	var input []json.RawMessage
	if json.Unmarshal(inputRaw, &input) != nil {
		return body, false
	}
	inputItems := make([]map[string]json.RawMessage, 0, len(input))
	for _, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) == nil {
			inputItems = append(inputItems, item)
		}
	}
	index := buildInputReplayIndex(inputItems)
	turns := groupReplayTurns(replayItems)
	pending := make(map[int][][]byte, len(turns))
	injected := 0
	for _, turn := range turns {
		anchor := turn.anchorIndex(inputItems)
		if anchor < 0 {
			continue
		}
		kept := turn.filterItems(index)
		if len(kept) == 0 {
			continue
		}
		pending[anchor] = append(pending[anchor], kept...)
		injected += len(kept)
	}
	if injected == 0 {
		return body, false
	}
	// 不基于不可信请求长度计算预分配容量,交由 append 安全扩容。
	next := make([]json.RawMessage, 0)
	for i, item := range input {
		for _, replay := range pending[i] {
			next = append(next, json.RawMessage(replay))
		}
		next = append(next, item)
	}
	for _, replay := range pending[len(input)] {
		next = append(next, json.RawMessage(replay))
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return body, false
	}
	payload["input"] = encoded
	updated, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return updated, true
}

// assistantSignature 生成 assistant 消息内容的规范化签名(轮内消息去重用)。
func assistantSignature(item map[string]json.RawMessage) string {
	parts, ok := assistantParts(item["content"])
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(part.partType)
		builder.WriteByte(0x1f)
		builder.WriteString(part.value)
		builder.WriteByte(0x1e)
	}
	return builder.String()
}

func assistantSignatureBytes(item []byte) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal(item, &raw) != nil {
		return ""
	}
	return assistantSignature(raw)
}

func lastAssistantMessage(items []map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		var typeName, role string
		_ = json.Unmarshal(item["type"], &typeName)
		_ = json.Unmarshal(item["role"], &role)
		if (strings.TrimSpace(typeName) == "" || strings.TrimSpace(typeName) == "message") && strings.EqualFold(strings.TrimSpace(role), "assistant") {
			return item, true
		}
	}
	return nil, false
}

func replayAssistantMessage(items [][]byte) (map[string]json.RawMessage, bool) {
	for _, item := range items {
		var raw map[string]json.RawMessage
		if json.Unmarshal(item, &raw) != nil {
			continue
		}
		var typeName, role string
		_ = json.Unmarshal(raw["type"], &typeName)
		_ = json.Unmarshal(raw["role"], &role)
		if strings.TrimSpace(typeName) == "message" && strings.EqualFold(strings.TrimSpace(role), "assistant") {
			return raw, true
		}
	}
	return nil, false
}

func assistantContentEqual(left, right map[string]json.RawMessage) bool {
	leftParts, leftOK := assistantParts(left["content"])
	rightParts, rightOK := assistantParts(right["content"])
	if !leftOK || !rightOK || len(leftParts) != len(rightParts) {
		return false
	}
	for i := range leftParts {
		if leftParts[i] != rightParts[i] {
			return false
		}
	}
	return true
}

type assistantPart struct {
	partType string
	value    string
}

func assistantParts(raw json.RawMessage) ([]assistantPart, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return []assistantPart{{partType: "output_text", value: asString}}, true
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, false
	}
	result := make([]assistantPart, 0, len(parts))
	for _, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		// Messages 协议重发的 assistant 文本被转换为 input_text,Responses
		// 原生重发为 output_text——两者是同一段文本的两种线形态,必须视为
		// 相等。历史回归:只认 output_text 时,Messages 客户端每一轮都被判
		// 定 assistant 不匹配,回放整体静默失效,上游缓存退化为公共头部。
		case "output_text", "input_text":
			var text string
			if json.Unmarshal(part["text"], &text) != nil {
				return nil, false
			}
			result = append(result, assistantPart{partType: "output_text", value: text})
		case "refusal":
			var refusal string
			if json.Unmarshal(part["refusal"], &refusal) != nil {
				return nil, false
			}
			result = append(result, assistantPart{partType: partType, value: refusal})
		default:
			return nil, false
		}
	}
	return result, len(result) > 0
}

func comparableReplayCallIDs(callID string) []string {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return nil
	}
	const anthropicPrefix = "toolu_"
	if strings.HasPrefix(callID, anthropicPrefix) {
		upstreamID := strings.TrimPrefix(callID, anthropicPrefix)
		if upstreamID != "" {
			return []string{callID, upstreamID}
		}
		return []string{callID}
	}
	return []string{callID, anthropicPrefix + callID}
}

func replayToolCallKeys(itemType, callID string) []string {
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return nil
	}
	ids := comparableReplayCallIDs(callID)
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, itemType+"\x00"+id)
	}
	return keys
}

func anyReplayCallKeyExists(existing map[string]bool, keys []string) bool {
	for _, key := range keys {
		if existing[key] {
			return true
		}
	}
	return false
}

func rewriteReplayCallID(item []byte, callID string) []byte {
	var raw map[string]json.RawMessage
	if json.Unmarshal(item, &raw) != nil {
		return item
	}
	encoded, err := json.Marshal(callID)
	if err != nil {
		return item
	}
	raw["call_id"] = encoded
	updated, err := json.Marshal(raw)
	if err != nil {
		return item
	}
	return updated
}
