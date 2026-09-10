package inference

import (
	"encoding/json"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"net/http"
	"strings"
)

const maxCodexTurnMetadataSize = 16 << 10

// extractClientSignals only decodes named wire fields. M12 chooses precedence,
// auxiliary isolation, stable identities and persistent replay eligibility.
func extractClientSignals(headers http.Header, body []byte) historydomain.ClientSignals {
	s := historydomain.ClientSignals{
		ClaudeSession: headers.Get("X-Claude-Code-Session-Id"), ClaudeAgent: headers.Get("X-Claude-Code-Agent-Id"),
		CodexTurn: parseTurnSignals(headers.Get("X-Codex-Turn-Metadata")), CodexWindow: headers.Get("X-Codex-Window-Id"),
		XSession: headers.Get("X-Session-Id"), Session: headers.Get("Session-Id"), SessionUnderscore: headers.Get("Session_id"),
		XConversation: headers.Get("X-Conversation-Id"), Conversation: headers.Get("Conversation-Id"), ConversationUnderscore: headers.Get("Conversation_id"),
		ClientSession: headers.Get("X-Client-Session-Id"), GrokConversation: headers.Get("X-Grok-Conv-Id"),
	}
	if s.ClaudeSession != "" {
		s.SystemTexts = parseSystemTexts(body)
	}
	var payload struct {
		PromptCacheKey      string `json:"prompt_cache_key"`
		ConversationID      string `json:"conversation_id"`
		ConversationIDCamel string `json:"conversationId"`
		SessionID           string `json:"session_id"`
		SessionIDCamel      string `json:"sessionId"`
		Metadata            struct {
			SessionID      string `json:"session_id"`
			SessionIDCamel string `json:"sessionId"`
			UserID         string `json:"user_id"`
		} `json:"metadata"`
		ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return s
	}
	s.PromptCacheKey = payload.PromptCacheKey
	s.MetadataSession, s.MetadataSessionCamel = payload.Metadata.SessionID, payload.Metadata.SessionIDCamel
	s.ClaudeUser = parseUserSessionSignals(payload.Metadata.UserID)
	s.ClientTurn = parseRawTurnSignals(payload.ClientMetadata["x-codex-turn-metadata"])
	if raw := payload.ClientMetadata["x-codex-window-id"]; len(raw) > 0 {
		var window string
		_ = json.Unmarshal(raw, &window)
		s.ClientWindow = window
	}
	s.BodySession, s.BodySessionCamel = payload.SessionID, payload.SessionIDCamel
	s.BodyConversation, s.BodyConversationCamel = payload.ConversationID, payload.ConversationIDCamel
	return s
}
func parseTurnSignals(value string) historydomain.TurnSignals {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxCodexTurnMetadataSize {
		return historydomain.TurnSignals{}
	}
	var metadata struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		WindowID       string `json:"window_id"`
	}
	if json.Unmarshal([]byte(value), &metadata) != nil {
		return historydomain.TurnSignals{}
	}
	return historydomain.TurnSignals{PromptCacheKey: metadata.PromptCacheKey, WindowID: metadata.WindowID}
}
func parseRawTurnSignals(raw json.RawMessage) historydomain.TurnSignals {
	if len(raw) == 0 {
		return historydomain.TurnSignals{}
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return parseTurnSignals(value)
	}
	return parseTurnSignals(string(raw))
}
func parseSystemTexts(body []byte) []string {
	var payload struct {
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.System) == 0 {
		return nil
	}
	var text string
	if json.Unmarshal(payload.System, &text) == nil {
		return []string{text}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(payload.System, &blocks) != nil {
		return nil
	}
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	return texts
}
func parseUserSessionSignals(userID string) historydomain.UserSessionSignals {
	userID = strings.TrimSpace(userID)
	var s historydomain.UserSessionSignals
	if userID == "" {
		return s
	}
	var embedded struct {
		SessionID      string `json:"session_id"`
		SessionIDCamel string `json:"sessionId"`
	}
	if json.Unmarshal([]byte(userID), &embedded) == nil {
		s.SessionID, s.SessionIDCamel = embedded.SessionID, embedded.SessionIDCamel
	}
	const marker = "_session_"
	if index := strings.LastIndex(userID, marker); index >= 0 {
		s.Suffix = userID[index+len(marker):]
	}
	return s
}
