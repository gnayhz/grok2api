package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

const buildSessionIdentityVersion = "v4"

type buildSessionIdentity struct {
	// upstreamID is sent as prompt_cache_key and x-grok-conv-id and must remain stable across turns.
	upstreamID string
	// affinityKey controls account stickiness and is isolated by model to avoid cross-model collisions.
	affinityKey string
	// replayKey is derived only from explicit client session signals; soft anchors must not drive encrypted reasoning replay.
	replayKey string
	// soft indicates a fallback identity derived from message content when no explicit session is available.
	soft bool
	// isolated indicates a Composer-only request identity used when the client
	// supplied no stable session.
	isolated bool
}

// bodyAnchors 惰性解析并记忆请求体的消息锚点(system+首条 user)。同一
// 请求内路由排序、预选候选、选中身份三处都要锚点,而解析是全量 JSON
// 扫描(128KB body 毫秒级)——只做一次。sync.Once 从属于创建它的那次
// 函数调用(局部变量),不跨请求共享。
type bodyAnchors struct {
	body      []byte
	once      sync.Once
	system    string
	firstUser string
}

func newBodyAnchors(body []byte) *bodyAnchors { return &bodyAnchors{body: body} }

func (a *bodyAnchors) load() (string, string) {
	a.once.Do(func() { a.system, a.firstUser, _ = extractMessageAnchors(a.body) })
	return a.system, a.firstUser
}

// resolveBuildSessionIdentity derives a stable Grok Build session identity:
// 1. Prefer explicit client session signals, isolated by client key, provider, and model.
// 2. Fall back to system/instructions and the first user message when no explicit signal exists.
// 3. Return an empty identity when no signal exists; never generate a random session ID per request.
func resolveBuildSessionIdentity(clientKeyID uint64, provider accountdomain.Provider, upstreamModel, explicitKey, sessionSeed, requestScope string, body []byte) buildSessionIdentity {
	return resolveBuildSessionIdentityWithAnchors(clientKeyID, provider, upstreamModel, explicitKey, sessionSeed, requestScope, newBodyAnchors(body))
}

func resolveBuildSessionIdentityWithAnchors(clientKeyID uint64, provider accountdomain.Provider, upstreamModel, explicitKey, sessionSeed, requestScope string, anchors *bodyAnchors) buildSessionIdentity {
	// Prefer Claude Code and Codex session signals extracted by the transport layer.
	// body.prompt_cache_key is only a fallback when no stronger header or session signal exists.
	seed := strings.TrimSpace(sessionSeed)
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if clientKeyID == 0 || provider == "" || model == "" {
		return buildSessionIdentity{}
	}
	if seed != "" {
		upstreamSource := fmt.Sprintf("grok2api:build-session:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		affinitySource := fmt.Sprintf("grok2api:build-affinity:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		replaySource := fmt.Sprintf("grok2api:build-replay:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		return buildSessionIdentity{
			upstreamID:  digestUUID(upstreamSource),
			affinityKey: hexDigest(affinitySource),
			replayKey:   hexDigest(replaySource),
		}
	}
	// Fall back to a message-prefix hash to keep account affinity and session IDs stable without client session signals.
	system, firstUser := anchors.load()
	firstUser = truncateAnchor(firstUser, 200)
	system = truncateAnchor(system, 100)
	if firstUser == "" {
		return buildSessionIdentity{}
	}
	// 软会话只保留账号亲和(affinityKey 稳定), 发往上游的会话 ID 按请求隔离:
	// 此前按「system+首条 user 前缀」合并, 开头雷同的不同对话会共享同一个上游
	// 会话, 上游历史互相串扰。客户端显式传 prompt_cache_key/session 的路径不受
	// 影响(它们的会话连续性来自显式信号)。
	// 长度前缀编码（v4）：system 与 firstUser 都是客户端可控文本，裸
	// ":%s:%s" 拼接存在移位歧义——system="a:b"+user="c" 与 system="a"
	// +user="b:c" 坍缩到同一 affinityKey（round 71 PoC 证实）。前缀长度
	// 使字段边界唯一，编码可逆。
	upstreamSource := fmt.Sprintf("grok2api:build-soft-session:%s:%d:%s:%s:%d:%s:%d:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, len(system), system, len(firstUser), firstUser, strings.TrimSpace(requestScope))
	affinitySource := fmt.Sprintf("grok2api:build-soft-affinity:%s:%d:%s:%s:%d:%s:%d:%s", buildSessionIdentityVersion, clientKeyID, provider, model, len(system), system, len(firstUser), firstUser)
	return buildSessionIdentity{
		upstreamID:  digestUUID(upstreamSource),
		affinityKey: hexDigest(affinitySource),
		soft:        true,
	}
}

// ensureBuildComposerSessionIdentity mirrors Composer's isolated-conversation
// requirement without copying CPA's per-attempt random UUID behavior. The
// request scope is stable across retries and route failover, while replay stays
// disabled because this is not an explicit client conversation.
func ensureBuildComposerSessionIdentity(identity buildSessionIdentity, clientKeyID uint64, provider accountdomain.Provider, upstreamModel, requestScope string) buildSessionIdentity {
	// Explicit client sessions and previous-response ownership are authoritative.
	// A soft message-prefix identity is intentionally replaced: two independent
	// Composer requests may begin with the same text and must not share a
	// conversation merely because their first message matches.
	if (identity.upstreamID != "" && !identity.soft) || clientKeyID == 0 || provider != accountdomain.ProviderBuild || !modeldomain.IsGrokComposerModel(upstreamModel) {
		return identity
	}
	requestScope = strings.TrimSpace(requestScope)
	if requestScope == "" {
		return identity
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	upstreamSource := fmt.Sprintf("grok2api:build-composer-isolated:v1:%d:%s:%s:%s", clientKeyID, provider, model, requestScope)
	affinitySource := fmt.Sprintf("grok2api:build-composer-affinity:v1:%d:%s:%s:%s", clientKeyID, provider, model, requestScope)
	identity.upstreamID = digestUUID(upstreamSource)
	identity.affinityKey = hexDigest(affinitySource)
	identity.replayKey = ""
	identity.soft = false
	identity.isolated = true
	return identity
}

func digestUUID(source string) string {
	digest := sha256.Sum256([]byte(source))
	hexID := hex.EncodeToString(digest[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[0:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:32])
}

func hexDigest(source string) string {
	digest := sha256.Sum256([]byte(source))
	return hex.EncodeToString(digest[:])
}

func truncateAnchor(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" || maxRunes <= 0 {
		return value
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes])
}

// extractMessageAnchors extracts stable prefix anchors from Chat, Messages, and Responses request bodies.
// It uses only system, the first user message, and an optional first assistant message to avoid hash drift across turns.
func extractMessageAnchors(body []byte) (system, firstUser, firstAssistant string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return "", "", ""
	}
	// Top-level system or instructions fields provide a stable system anchor for OpenAI Responses and Chat.
	if raw, ok := root["instructions"]; ok {
		system = flattenMessageContent(raw)
	}
	if system == "" {
		if raw, ok := root["system"]; ok {
			system = flattenMessageContent(raw)
		}
	}
	if raw, ok := root["messages"]; ok {
		msgSystem, msgUser, msgAssistant := anchorsFromRoleMessages(raw)
		if system == "" {
			system = msgSystem
		}
		firstUser, firstAssistant = msgUser, msgAssistant
		if firstUser != "" {
			return system, firstUser, firstAssistant
		}
	}
	if raw, ok := root["input"]; ok {
		inSystem, inUser, inAssistant := anchorsFromResponsesInput(raw)
		if system == "" {
			system = inSystem
		}
		if firstUser == "" {
			firstUser = inUser
		}
		if firstAssistant == "" {
			firstAssistant = inAssistant
		}
	}
	return system, firstUser, firstAssistant
}

func anchorsFromRoleMessages(raw json.RawMessage) (system, firstUser, firstAssistant string) {
	var messages []map[string]json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return "", "", ""
	}
	for _, msg := range messages {
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		content := flattenMessageContent(msg["content"])
		if content == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system":
			if system == "" {
				system = content
			}
		case "user":
			if firstUser == "" {
				firstUser = content
			}
		case "assistant":
			if firstAssistant == "" {
				firstAssistant = content
			}
		}
		if system != "" && firstUser != "" && firstAssistant != "" {
			break
		}
	}
	return system, firstUser, firstAssistant
}

func anchorsFromResponsesInput(raw json.RawMessage) (system, firstUser, firstAssistant string) {
	// Shorthand form: input is a direct string.
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return "", strings.TrimSpace(asString), ""
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return "", "", ""
	}
	for _, item := range items {
		var typeName, role string
		_ = json.Unmarshal(item["type"], &typeName)
		_ = json.Unmarshal(item["role"], &role)
		typeName = strings.TrimSpace(typeName)
		role = strings.ToLower(strings.TrimSpace(role))
		// Top-level instructions handle the system anchor; this branch extracts messages.
		if typeName != "" && typeName != "message" {
			continue
		}
		content := flattenMessageContent(item["content"])
		if content == "" {
			// Support content objects whose text field is a string.
			var text string
			if json.Unmarshal(item["text"], &text) == nil {
				content = strings.TrimSpace(text)
			}
		}
		if content == "" {
			continue
		}
		switch role {
		case "system", "developer":
			if system == "" {
				system = content
			}
		case "user":
			if firstUser == "" {
				firstUser = content
			}
		case "assistant":
			if firstAssistant == "" {
				firstAssistant = content
			}
		default:
			// Treat role-less plain-text input items as user input.
			if role == "" && firstUser == "" && (typeName == "" || typeName == "message") {
				firstUser = content
			}
		}
		if firstUser != "" && firstAssistant != "" {
			break
		}
	}
	// Use top-level instructions as a system fallback.
	return system, firstUser, firstAssistant
}

func flattenMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return strings.TrimSpace(asString)
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		case "", "text", "input_text", "output_text":
			var text string
			if json.Unmarshal(part["text"], &text) == nil && strings.TrimSpace(text) != "" {
				if builder.Len() > 0 {
					builder.WriteByte('\n')
				}
				builder.WriteString(strings.TrimSpace(text))
			}
		}
	}
	return builder.String()
}
