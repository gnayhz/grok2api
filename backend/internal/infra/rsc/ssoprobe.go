package rsc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/bogdanfinn/websocket"
)

// Probe is the transport contract behind every RSC check method. The legacy
// homepage parser (*Checker) and the SSO thinking probe (*SSOProbeChecker)
// both project onto it so the risk service stays method-agnostic.
type Probe interface {
	Check(ctx context.Context, ssoToken string) Result
}

var (
	_ Probe = (*Checker)(nil)
	_ Probe = (*SSOProbeChecker)(nil)
)

// 2026-08: grok.com stopped delivering the botFlag fields through the Next.js
// RSC payload, so the homepage check reads every account as clean. The only
// surviving account-level signal is the reasoning stream itself: a healthy
// Web SSO identity receives a CHANNEL_ASSISTANT_NOTETAKER_HEADER chunk
// ("Thinking about your request") - or CHANNEL_REASONING - before any answer
// text, while a risk-controlled (degraded) account gets neither and the
// CHANNEL_ASSISTANT_RESPONSE text arrives directly.
//
// Cost note: one probe sends one tiny "fast" message through a temporary,
// memory-disabled session, so it consumes one message of the account's
// rolling quota. Event-driven attribution, the singleflight merge, and the
// admission cap keep that volume bounded.
const (
	probeDefaultModel   = "fast" // browser short model name; pairs with the inlined response.create send style
	probeDefaultPrompt  = "OK"
	probeUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	probeMaxFrameBytes  = 16 << 20
	probeAnswerSnippets = 60 // runes of answer text kept in BotFlagDetails
)

// Channel-vocabulary circuit breaker. "Answer text with no thinking" is only
// proof of risk while the thinking channel names are still what we think they
// are: if xai renames CHANNEL_ASSISTANT_NOTETAKER_HEADER but keeps
// CHANNEL_ASSISTANT_RESPONSE, every healthy account would read as denied and
// the whole pool would be flagged rsc_denied (a permanent, operator-cleared
// flag). Rule: a denied verdict is only trusted while the recent probe window
// has seen at least one clean (thinking observed proves the channel names are
// alive); otherwise it degrades to error (retried later, never flagged). A
// genuinely degraded cohort simply flags a bit later, once any healthy
// identity elsewhere in the pool is probed clean.
const (
	probeVitalityWindow     = 32 // last N clean/denied classifications considered
	probeVitalityMinDenials = 8  // >= this many denials with zero cleans trips the breaker
)

// Channel names observed on the mgw response.chunk stream.
const (
	channelNotetakerHeader = "CHANNEL_ASSISTANT_NOTETAKER_HEADER"
	channelReasoning       = "CHANNEL_REASONING"
	channelAssistantText   = "CHANNEL_ASSISTANT_RESPONSE"
)

// SSOProbeChecker detects registration risk by opening one real mgw
// conversation with the SSO cookie and watching for the reasoning stream.
// It is a Go port of the regc data/go-detect example, wired onto the same
// Result/verdict vocabulary as the legacy homepage checker. Direct (no-proxy)
// browser-TLS access is fine: the signal is account-level, not IP-level.
type SSOProbeChecker struct {
	// baseURL overrides https://grok.com for tests (empty means production).
	baseURL string
	Timeout time.Duration
	// Model is the browser short model name (default "fast").
	Model string
	// Prompt is the probe message body (default "OK").
	Prompt string
	// vitality tracks the recent clean/denied mix; see the breaker comment
	// above. One checker instance lives for the process, so the window
	// survives across checks and restarts simply reset it.
	vitality probeVitality
}

// probeVitality is a small sliding window over clean (true) / denied (false)
// probe verdicts. Errors do not enter the window: an unreachable upstream
// must neither trip nor heal the breaker.
type probeVitality struct {
	mu      sync.Mutex
	recent  []bool
	cleans  int
	denials int
}

// record stores one clean/denied classification, evicting beyond the window.
func (v *probeVitality) record(clean bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.recent) >= probeVitalityWindow {
		if v.recent[0] {
			v.cleans--
		} else {
			v.denials--
		}
		v.recent = v.recent[1:]
	}
	v.recent = append(v.recent, clean)
	if clean {
		v.cleans++
	} else {
		v.denials++
	}
}

// suspect reports whether the channel vocabulary looks broken: enough
// denials accumulated without a single clean witness.
func (v *probeVitality) suspect() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.cleans == 0 && v.denials >= probeVitalityMinDenials
}

// NewSSOProbeChecker builds a probe checker; timeout<=0 falls back to 45s.
func NewSSOProbeChecker(timeout time.Duration) *SSOProbeChecker {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &SSOProbeChecker{Timeout: timeout, Model: probeDefaultModel, Prompt: probeDefaultPrompt}
}

// Check runs one thinking probe. Verdicts:
//   - clean : notetaker header or reasoning chunk seen -> account healthy
//   - denied: answer text arrived with no thinking     -> risk-controlled
//   - error : transport/stream trouble (rate limits, challenges, timeouts).
//     Never treated as clean or denied; callers retry later.
//
// Transient statuses (403/429/502/503) retry once on a fresh client, mirroring
// the legacy homepage checker.
func (p *SSOProbeChecker) Check(ctx context.Context, ssoToken string) Result {
	ssoToken = strings.TrimSpace(ssoToken)
	if ssoToken == "" {
		return Result{Verdict: VerdictError, Error: "empty sso token", CheckedAt: time.Now().UTC()}
	}
	result := p.attempt(ctx, ssoToken)
	if result.Verdict == VerdictError && transientHTTPStatus(result.HTTPStatus) {
		select {
		case <-ctx.Done():
			return result
		case <-time.After(1200 * time.Millisecond):
		}
		retried := p.attempt(ctx, ssoToken)
		if retried.Verdict != VerdictError || !transientHTTPStatus(retried.HTTPStatus) {
			result = retried
		}
	}
	return p.vitalityGate(result)
}

// vitalityGate records every decidable clean/denied verdict and downgrades
// denied to error while the thinking-channel vocabulary looks broken. Clean
// is always trusted (a thinking chunk is direct evidence the channels are
// alive) and heals the breaker.
func (p *SSOProbeChecker) vitalityGate(result Result) Result {
	switch result.Verdict {
	case VerdictClean:
		p.vitality.record(true)
	case VerdictDenied:
		p.vitality.record(false)
		if p.vitality.suspect() {
			downgraded := Result{
				Verdict:        VerdictError,
				HTTPStatus:     result.HTTPStatus,
				BotFlagDetails: result.BotFlagDetails,
				CheckedAt:      result.CheckedAt,
				// Suppressed marks the channel-vocabulary breaker downgrade so
				// callers can attempt witness re-validation (see the risk
				// service) instead of retrying blindly.
				Suppressed: true,
			}
			downgraded.Error = fmt.Sprintf("denied suppressed: no thinking channel observed in the last %d probes (channel vocabulary suspect)", probeVitalityMinDenials)
			return downgraded
		}
	}
	return result
}

func transientHTTPStatus(status int) bool {
	switch status {
	case 403, 429, 502, 503:
		return true
	}
	return false
}

func (p *SSOProbeChecker) newClient() (tlsclient.HttpClient, error) {
	return tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
		tlsclient.WithTimeoutSeconds(int(p.Timeout.Seconds())+1),
		tlsclient.WithClientProfile(profiles.Chrome_146),
		tlsclient.WithNotFollowRedirects(),
	)
}

func (p *SSOProbeChecker) attempt(ctx context.Context, ssoToken string) Result {
	now := time.Now().UTC()
	client, err := p.newClient()
	if err != nil {
		return Result{Verdict: VerdictError, Error: fmt.Sprintf("build client: %v", err), CheckedAt: now}
	}
	defer client.CloseIdleConnections()
	base := strings.TrimRight(strings.TrimSpace(p.baseURL), "/")
	if base == "" {
		base = "https://grok.com"
	}
	// 1. resolve the userId bound to the SSO cookie.
	userID, status, err := p.fetchSessionUserID(ctx, client, base, ssoToken)
	if err != nil {
		return Result{Verdict: VerdictError, HTTPStatus: status, Error: fmt.Sprintf("session: %v", err), CheckedAt: now}
	}
	// 2. dial the gateway with full browser headers over the same
	// browser-TLS client (fingerprint parity with the chat gateway).
	endpoint, origin, err := probeGatewayEndpoint(base, userID)
	if err != nil {
		return Result{Verdict: VerdictError, Error: fmt.Sprintf("gateway endpoint: %v", err), CheckedAt: now}
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout:  p.Timeout,
		NetDialTLSContext: client.GetTLSDialer(),
		NetDialContext:    client.GetDialer().DialContext,
	}
	header := fhttp.Header{}
	header.Set("Cookie", fmt.Sprintf("sso=%s; sso-rw=%s; x-userid=%s", ssoToken, ssoToken, userID))
	header.Set("Origin", origin)
	header.Set("User-Agent", probeUserAgent)
	header.Set("Accept-Language", "en-US,en;q=0.9")
	header.Set("Cache-Control", "no-cache")
	header.Set("Pragma", "no-cache")
	connection, handshake, dialErr := dialer.DialContext(ctx, endpoint, header)
	if dialErr != nil {
		status := 0
		if handshake != nil {
			status = handshake.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(handshake.Body, 4<<10))
			_ = handshake.Body.Close()
		}
		return Result{Verdict: VerdictError, HTTPStatus: status, Error: fmt.Sprintf("dial gateway: %v", dialErr), CheckedAt: now}
	}
	defer connection.Close()
	connection.SetReadLimit(probeMaxFrameBytes)
	_ = connection.SetReadDeadline(time.Now().Add(p.Timeout))
	return p.runConversation(connection)
}

// fetchSessionUserID reads /api/auth/session with the SSO cookie. An
// unauthenticated or blocked session is a credential problem, not a risk
// verdict, so it maps onto error (retry/later), never clean or denied.
func (p *SSOProbeChecker) fetchSessionUserID(ctx context.Context, client tlsclient.HttpClient, base, ssoToken string) (string, int, error) {
	request, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, base+"/api/auth/session", nil)
	if err != nil {
		return "", 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", probeUserAgent)
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Cookie", fmt.Sprintf("sso=%s; sso-rw=%s", ssoToken, ssoToken))
	response, err := client.Do(request)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return "", response.StatusCode, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload struct {
		Status  string `json:"status"`
		Session struct {
			UserID string `json:"userId"`
		} `json:"session"`
		UserID string `json:"userId"`
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return "", response.StatusCode, err
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", response.StatusCode, fmt.Errorf("parse session: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(payload.Status)) {
	case "unauthenticated", "blocked":
		return "", response.StatusCode, fmt.Errorf("session status %s", payload.Status)
	}
	userID := strings.TrimSpace(payload.Session.UserID)
	if userID == "" {
		userID = strings.TrimSpace(payload.UserID)
	}
	if userID == "" {
		return "", response.StatusCode, fmt.Errorf("session response missing userId")
	}
	return userID, response.StatusCode, nil
}

// probeGatewayEndpoint mirrors the chat gateway's endpoint shape:
// {base}/ws/mgw/?uid=<userID>, with ws(s) derived from http(s).
func probeGatewayEndpoint(base, userID string) (string, string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("base URL invalid: %q", base)
	}
	origin := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", "", fmt.Errorf("base URL scheme invalid: %q", parsed.Scheme)
	}
	parsed.Path = "/ws/mgw/"
	parsed.RawPath = ""
	parsed.RawQuery = url.Values{"uid": []string{userID}}.Encode()
	parsed.Fragment = ""
	return parsed.String(), origin, nil
}

// probeOutcome accumulates the classification state of one conversation.
type probeOutcome struct {
	notetakerHeaders int
	reasoningChunks  int
	answerText       strings.Builder
	servedModel      string
	streamError      string
}

// runConversation drives one temporary mgw session: session.create, the user
// turn inlined in response.create after conversation.attached, then classify
// on the first decidable response.chunk. A healthy account emits the notetaker
// header (or reasoning) before any answer text; a degraded account emits the
// answer directly. Stream errors stay inconclusive (error verdict) so rate
// limits can never masquerade as risk.
func (p *SSOProbeChecker) runConversation(connection *websocket.Conn) Result {
	now := time.Now().UTC()
	outcome := &probeOutcome{}
	model := p.Model
	if strings.TrimSpace(model) == "" {
		model = probeDefaultModel
	}
	prompt := p.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = probeDefaultPrompt
	}
	writeDeadline := func() { _ = connection.SetWriteDeadline(time.Now().Add(15 * time.Second)) }

	// session.create: temporary, memory/artifact-disabled, no follow-ups -
	// the probe must not leave junk conversations on production accounts.
	writeDeadline()
	create := map[string]any{"event": map[string]any{
		"type":     "session.create",
		"event_id": fmt.Sprintf("evt_init_%d", time.Now().UnixNano()),
		"session": map[string]any{
			"model": model,
			"x_grok": map[string]any{
				"protocol_capabilities":   []string{"conversation_attached", "custom_methods_v1"},
				"use_chunk":               true,
				"enable_side_by_side":     true,
				"force_side_by_side":      false,
				"enable_image_generation": true,
				"image_generation_count":  2,
				"disable_text_follow_ups": false,
				"disable_artifact":        true,
				"force_concise":           false,
				"keep_context":            false,
				"is_temporary":            true,
				"disable_memory":          true,
			},
		},
	}}
	if err := connection.WriteJSON(create); err != nil {
		return Result{Verdict: VerdictError, Error: fmt.Sprintf("send session.create: %v", err), CheckedAt: now}
	}

	sent := false
	for {
		var envelope map[string]any
		if err := connection.ReadJSON(&envelope); err != nil {
			// Timeout / closed before a decidable chunk: inconclusive.
			outcome.streamError = fmt.Sprintf("stream ended before decision: %v", err)
			return outcome.finish(now)
		}
		event, _ := envelope["event"].(map[string]any)
		eventType, _ := event["type"].(string)
		sessionID, _ := envelope["session_id"].(string)

		switch eventType {
		case "conversation.attached":
			if !sent {
				sent = true
				// Browser split-send style: user message inlined in response.create.
				millis := time.Now().UnixMilli()
				turn := map[string]any{
					"session_id": sessionID,
					"event": map[string]any{
						"type":     "response.create",
						"event_id": fmt.Sprintf("evt_resp_%d", millis),
						"item": map[string]any{
							"type": "message", "role": "user",
							"x_grok": map[string]any{
								// 32-hex client message id; any stable unique id works.
								"client_message_id": fmt.Sprintf("%032x", millis),
								"input_chunks":      []any{map[string]any{"text": map[string]any{"text": prompt}}},
							},
						},
					},
				}
				writeDeadline()
				if err := connection.WriteJSON(turn); err != nil {
					return Result{Verdict: VerdictError, Error: fmt.Sprintf("send response.create: %v", err), CheckedAt: now}
				}
			}

		case "response.created":
			if response, ok := event["response"].(map[string]any); ok {
				if xGrok, ok := response["x_grok"].(map[string]any); ok {
					if served, ok := xGrok["model"].(string); ok && served != "" {
						outcome.servedModel = served
					}
				}
			}

		case "response.chunk":
			chunk, _ := event["chunk"].(map[string]any)
			text, _ := chunk["text"].(map[string]any)
			if text == nil {
				continue
			}
			channel, _ := text["channel"].(string)
			switch channel {
			case channelNotetakerHeader:
				outcome.notetakerHeaders++
				return outcome.finish(now) // decidable: healthy
			case channelReasoning:
				outcome.reasoningChunks++
				return outcome.finish(now) // decidable: healthy
			case channelAssistantText:
				if value, ok := text["text"].(string); ok {
					outcome.answerText.WriteString(value)
				}
				return outcome.finish(now) // decidable: answered with no thinking
			}

		case "response.grok.output":
			if output, ok := event["output"].(map[string]any); ok {
				if streamError, ok := output["stream_error"]; ok && streamError != nil {
					if raw, err := json.Marshal(streamError); err == nil {
						outcome.streamError = string(raw)
					} else {
						outcome.streamError = "stream_error"
					}
					return outcome.finish(now) // inconclusive: retry later
				}
			}

		case "error":
			if raw, err := json.Marshal(event["error"]); err == nil {
				outcome.streamError = string(raw)
			} else {
				outcome.streamError = "gateway error event"
			}
			return outcome.finish(now)

		case "response.done", "session.ended":
			outcome.streamError = "stream ended (" + eventType + ") before a decidable chunk"
			return outcome.finish(now)
		}
	}
}

// finish classifies the accumulated outcome onto the shared Result shape.
func (o *probeOutcome) finish(checkedAt time.Time) Result {
	result := Result{CheckedAt: checkedAt}
	detail := fmt.Sprintf("probe model=%s notetaker=%d reasoning=%d", o.servedModel, o.notetakerHeaders, o.reasoningChunks)
	switch {
	case o.streamError != "":
		result.Verdict = VerdictError
		result.Error = o.streamError
		result.BotFlagDetails = "sso probe inconclusive: " + detail
	case o.notetakerHeaders > 0 || o.reasoningChunks > 0:
		result.Verdict = VerdictClean
		result.BotFlagDetails = "sso probe thinking_ok: " + detail
	case o.answerText.Len() > 0:
		result.Verdict = VerdictDenied
		answer := []rune(o.answerText.String())
		if len(answer) > probeAnswerSnippets {
			answer = answer[:probeAnswerSnippets]
		}
		result.BotFlagDetails = fmt.Sprintf("sso probe answer_without_thinking: %s answer=%q", detail, string(answer))
	default:
		// Neither channel arrived nor an explicit error: inconclusive.
		result.Verdict = VerdictError
		result.Error = "probe closed without thinking or answer chunks"
		result.BotFlagDetails = "sso probe inconclusive: " + detail
	}
	return result
}
