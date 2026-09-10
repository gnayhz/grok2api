package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
)

const buildSessionIdentityVersion = "v5"

// Identity separates the upstream prefix-cache hint, account affinity and the
// explicit-session replay seed. A soft or isolated hint never authorizes replay.
type Identity struct {
	UpstreamID     string
	AffinityKey    string
	ReplayKey      string
	PriorReplayKey string
	Soft           bool
	Isolated       bool
}

// IdentityTarget contains model and Provider facts supplied by the caller. M05
// decides which products require isolated requests; M12 applies that fact.
type IdentityTarget struct {
	Provider               string
	Model                  string
	IsolatedWithoutSession bool
}

// IdentityRequest freezes one logical request's signals and lazy message anchors.
// It must not be reused with a different body or request scope.
type IdentityRequest struct {
	clientKeyID                              uint64
	seed, priorSeed, requestID, requestScope string
	anchors                                  *bodyAnchors
}

func NewIdentityRequest(clientKeyID uint64, signals historydomain.ClientSignals, fallbackKey, requestID, requestScope string, body []byte) *IdentityRequest {
	seed := historydomain.ResolveClientSeed(signals)
	if seed.Key == "" {
		seed = historydomain.RawClientSeed(fallbackKey)
	}
	return &IdentityRequest{clientKeyID: clientKeyID, seed: seed.Key, priorSeed: seed.PriorKey, requestID: requestID, requestScope: requestScope, anchors: newBodyAnchors(body)}
}

// RouteSeed is a soft route-ordering hint. It grants neither durable history nor
// account or network eligibility. Its existing byte format remains compatible.
func (r *IdentityRequest) RouteSeed() string {
	anchor := r.seed
	if anchor == "" {
		system, firstUser, _ := r.anchors.load()
		system = truncateAnchor(system, 100)
		firstUser = truncateAnchor(firstUser, 200)
		if firstUser != "" {
			anchor = "Soft:" + system + ":" + firstUser
		}
	}
	if anchor == "" {
		anchor = strings.TrimSpace(r.requestID)
	}
	return fmt.Sprintf("%d:%s", r.clientKeyID, anchor)
}

// IdentityResolver owns bounded, instance-local soft-conversation hints. Its zero
// value is ready for use, has no worker, and needs no shutdown or persistence.
// Persisted explicit history remains independent of this optional hint table.
type IdentityResolver struct{ soft softConversationRegistry }

func (r *IdentityResolver) Resolve(request *IdentityRequest, target IdentityTarget, inherited Identity) Identity {
	identity := inherited
	if identity.UpstreamID == "" {
		identity = r.resolve(request, target)
	}
	return ensureIsolatedIdentity(identity, request.clientKeyID, target, request.requestScope)
}

// bodyAnchors 惰性解析并记忆请求体的消息锚点(system+首条 user)。同一
// 请求内路由排序、预选候选、选中身份三处都要锚点,而解析是全量 JSON
// 扫描(128KB body 毫秒级)——只做一次。sync.Once 从属于创建它的那次
// 函数调用(局部变量),不跨请求共享。
type bodyAnchors struct {
	body           []byte
	once           sync.Once
	system         string
	firstUser      string
	firstAssistant string
}

func newBodyAnchors(body []byte) *bodyAnchors { return &bodyAnchors{body: body} }

func (a *bodyAnchors) load() (string, string, string) {
	a.once.Do(func() { a.system, a.firstUser, a.firstAssistant = extractMessageAnchors(a.body) })
	return a.system, a.firstUser, a.firstAssistant
}

func (r *IdentityResolver) resolve(request *IdentityRequest, target IdentityTarget) Identity {
	clientKeyID, provider, upstreamModel := request.clientKeyID, target.Provider, target.Model
	seed, anchors := request.seed, request.anchors
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if clientKeyID == 0 || provider == "" || model == "" {
		return Identity{}
	}
	if seed != "" {
		version := buildSessionIdentityVersion
		if request.priorSeed != "" {
			version = "v6"
		}
		upstreamSource := fmt.Sprintf("grok2api:build-session:%s:%d:%s:%s:%s", version, clientKeyID, provider, model, seed)
		affinitySource := fmt.Sprintf("grok2api:build-affinity:%s:%d:%s:%s:%s", version, clientKeyID, provider, model, seed)
		replaySource := fmt.Sprintf("grok2api:build-replay:%s:%d:%s:%s:%s", version, clientKeyID, provider, model, seed)
		priorReplayKey := ""
		if request.priorSeed != "" {
			priorReplayKey = hexDigest(fmt.Sprintf("grok2api:build-replay:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, request.priorSeed))
		}
		return Identity{
			PriorReplayKey: priorReplayKey,
			UpstreamID:     digestUUID(upstreamSource),
			AffinityKey:    hexDigest(affinitySource),
			ReplayKey:      hexDigest(replaySource),
		}
	}
	// Fall back to message anchors plus the soft-conversation registry: the
	// conversation key stays deterministic per opening (cache-warm from turn 1)
	// while divergent conversations with the same opening get forked keys.
	system, firstUser, firstAssistant := anchors.load()
	firstUser = truncateAnchor(firstUser, 200)
	system = truncateAnchor(system, 100)
	firstAssistant = truncateAnchor(firstAssistant, 512)
	if firstUser == "" {
		return Identity{}
	}
	// 长度前缀编码:锚点均为客户端可控文本,裸 ":%s:%s" 拼接存在移位
	// 歧义,前缀长度使字段边界唯一、编码可逆。
	openingSig := hexDigest(fmt.Sprintf("grok2api:build-soft-opening:%s:%d:%s:%s:%d:%s:%d:%s", buildSessionIdentityVersion, clientKeyID, provider, model, len(system), system, len(firstUser), firstUser))
	fullSig := openingSig
	if firstAssistant != "" {
		fullSig = hexDigest(fmt.Sprintf("grok2api:build-soft-full:%s:%d:%s:%s:%d:%s:%d:%s:%d:%s", buildSessionIdentityVersion, clientKeyID, provider, model, len(system), system, len(firstUser), firstUser, len(firstAssistant), firstAssistant))
	}
	// 基线键只由开场白决定:同一对话从第 1 轮起键恒定,首轮写入的缓存在
	// 第 2 轮即被命中(不存在此前按回复派生时第 1→2 轮的键轮换)。分叉键
	// 由完整签名决定,同开场白的不同对话在第 2 轮被登记表识别并隔离;
	// 两个键均确定性派生,进程重启/登记表丢失后原样重建。
	openingConvID := digestUUID("grok2api:build-soft-session:" + buildSessionIdentityVersion + ":base:" + openingSig)
	forkConvID := digestUUID("grok2api:build-soft-session:" + buildSessionIdentityVersion + ":fork:" + fullSig)
	upstreamID := r.soft.resolveSoftConversation(openingSig, fullSig, openingConvID, forkConvID)
	affinitySource := fmt.Sprintf("grok2api:build-soft-affinity:%s:%d:%s:%s:%d:%s:%d:%s", buildSessionIdentityVersion, clientKeyID, provider, model, len(system), system, len(firstUser), firstUser)
	return Identity{
		UpstreamID:  upstreamID,
		AffinityKey: hexDigest(affinitySource),
		Soft:        true,
	}
}

// ensureIsolatedIdentity mirrors Composer's isolated-conversation
// requirement without copying CPA's per-attempt random UUID behavior. The
// request scope is stable across retries and route failover, while replay stays
// disabled because this is not an explicit client conversation.
func ensureIsolatedIdentity(identity Identity, clientKeyID uint64, target IdentityTarget, requestScope string) Identity {
	// Explicit client sessions and previous-response ownership are authoritative.
	// A soft message-prefix identity is intentionally replaced: two independent
	// Composer requests may begin with the same text and must not share a
	// conversation merely because their first message matches.
	provider, upstreamModel := target.Provider, target.Model
	if (identity.UpstreamID != "" && !identity.Soft) || clientKeyID == 0 || !target.IsolatedWithoutSession {
		return identity
	}
	requestScope = strings.TrimSpace(requestScope)
	if requestScope == "" {
		return identity
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	upstreamSource := fmt.Sprintf("grok2api:build-composer-isolated:v1:%d:%s:%s:%s", clientKeyID, provider, model, requestScope)
	affinitySource := fmt.Sprintf("grok2api:build-composer-affinity:v1:%d:%s:%s:%s", clientKeyID, provider, model, requestScope)
	identity.UpstreamID = digestUUID(upstreamSource)
	identity.AffinityKey = hexDigest(affinitySource)
	identity.ReplayKey = ""
	identity.Soft = false
	identity.Isolated = true
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
