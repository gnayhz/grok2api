package history

import (
	"strconv"
	"strings"
)

const MaxClientSeedBytes = 1024

// ClientSignals carries parsed protocol facts. Neither signal extraction nor
// the transport chooses a session, auxiliary scope, or replay eligibility.
type ClientSignals struct {
	PromptCacheKey                                                         string
	ClaudeSession, ClaudeAgent                                             string
	CodexTurn                                                              TurnSignals
	CodexWindow                                                            string
	XSession, Session, SessionUnderscore                                   string
	XConversation, Conversation, ConversationUnderscore                    string
	ClientSession, GrokConversation                                        string
	MetadataSession, MetadataSessionCamel                                  string
	ClaudeUser                                                             UserSessionSignals
	ClientTurn                                                             TurnSignals
	ClientWindow                                                           string
	BodySession, BodySessionCamel, BodyConversation, BodyConversationCamel string
	SystemTexts                                                            []string
}
type TurnSignals struct{ PromptCacheKey, WindowID string }
type UserSessionSignals struct{ SessionID, SessionIDCamel, Suffix string }

// ResolveClientSeed is the single precedence and auxiliary-isolation policy.
// Body request keys identify branches inside coarser windows. Claude title
// requests without their own branch remain soft-only; explicit title keys are
// namespaced separately from main reasoning history.
type ClientSeed struct {
	Key string
	// PriorKey is an ambiguous old identity, usable only for read-only loss checks.
	PriorKey string
}

func ResolveClientSeed(s ClientSignals) ClientSeed {
	explicit := normalizeClientSeed(s.PromptCacheKey)
	if isClaudeTitle(s) {
		if explicit != "" {
			return namedClientSeed("title", "aux:claude-title:"+explicit, explicit)
		}
		return ClientSeed{}
	}
	if explicit != "" {
		return RawClientSeed(explicit)
	}
	if seed := normalizeClientSeed(s.ClaudeSession); seed != "" {
		return claudeSeed(seed, s.ClaudeAgent)
	}
	if seed := turnSeed(s.CodexTurn); seed.Key != "" {
		return seed
	}
	if seed := normalizeClientSeed(s.CodexWindow); seed != "" {
		return namedClientSeed("window", "codex:window:"+seed, seed)
	}
	for _, value := range []string{s.XSession, s.Session, s.SessionUnderscore, s.XConversation, s.Conversation, s.ConversationUnderscore, s.ClientSession, s.GrokConversation, s.MetadataSession, s.MetadataSessionCamel} {
		if seed := normalizeClientSeed(value); seed != "" {
			return RawClientSeed(seed)
		}
	}
	for _, value := range []string{s.ClaudeUser.SessionID, s.ClaudeUser.SessionIDCamel, s.ClaudeUser.Suffix} {
		if seed := normalizeClientSeed(value); seed != "" {
			return claudeSeed(seed, s.ClaudeAgent)
		}
	}
	if seed := turnSeed(s.ClientTurn); seed.Key != "" {
		return seed
	}
	if seed := normalizeClientSeed(s.ClientWindow); seed != "" {
		return namedClientSeed("window", "codex:window:"+seed, seed)
	}
	for _, value := range []string{s.BodySession, s.BodySessionCamel, s.BodyConversation, s.BodyConversationCamel} {
		if seed := normalizeClientSeed(value); seed != "" {
			return RawClientSeed(seed)
		}
	}
	return ClientSeed{}
}
func turnSeed(s TurnSignals) ClientSeed {
	if seed := normalizeClientSeed(s.PromptCacheKey); seed != "" {
		return RawClientSeed(seed)
	}
	if seed := normalizeClientSeed(s.WindowID); seed != "" {
		return namedClientSeed("window", "codex:window:"+seed, seed)
	}
	return ClientSeed{}
}
func claudeSeed(session, agent string) ClientSeed {
	agent = normalizeClientSeed(agent)
	if agent == "" {
		agent = "main"
	}
	return namedClientSeed("claude", "claude:"+session+":agent:"+agent, session, agent)
}
func normalizeClientSeed(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > MaxClientSeedBytes {
		return ""
	}
	return value
}
func isClaudeTitle(s ClientSignals) bool {
	if normalizeClientSeed(s.ClaudeSession) == "" {
		return false
	}
	for _, value := range s.SystemTexts {
		value = strings.ToLower(strings.TrimSpace(value))
		if strings.Contains(value, "generate a concise") && strings.Contains(value, "title") && strings.Contains(value, "coding session") {
			return true
		}
	}
	return false
}

// Lengths count bytes, so arbitrary separators and Unicode remain unambiguous.
// All named variants live under one reserved prefix. Raw keys in any old or new
// reserved namespace are escaped, preventing clients from impersonating a name.
const clientSeedPrefix = "g2a:identity:1:"

func namedClientSeed(kind, prior string, fields ...string) ClientSeed {
	var b strings.Builder
	b.WriteString(clientSeedPrefix)
	b.WriteString(kind)
	for _, field := range fields {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(len(field)))
		b.WriteByte(':')
		b.WriteString(field)
	}
	return ClientSeed{Key: b.String(), PriorKey: prior}
}
func RawClientSeed(value string) ClientSeed {
	value = normalizeClientSeed(value)
	if value == "" {
		return ClientSeed{}
	}
	for _, prefix := range []string{"claude:", "codex:window:", "aux:claude-title:", clientSeedPrefix} {
		if strings.HasPrefix(value, prefix) {
			return namedClientSeed("raw", value, value)
		}
	}
	return ClientSeed{Key: value}
}
