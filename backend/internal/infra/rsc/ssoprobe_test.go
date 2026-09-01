package rsc

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
)

const probeTestUserID = "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"

const probeSessionBody = "{\"session\":{\"userId\":\"" + probeTestUserID + "\",\"email\":\"probe@example.com\"}}"

// probeServer drives one fake grok.com serving /api/auth/session plus the
// mgw websocket. chunks is the scripted gateway event sequence sent after
// the client response.create arrives.
type probeServerScript struct {
	sessionStatus int
	sessionBody   string
	handshakeFail int // non-zero: /ws/mgw/ answers this status without upgrading
	// delay 非 0:发送 chunks 前静默该时长(驱动客户端读超时路径)。
	delay  time.Duration
	chunks []map[string]any
}

func probeServer(t *testing.T, script *probeServerScript) *fhttptest.Server {
	t.Helper()
	return fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		switch request.URL.Path {
		case "/api/auth/session":
			status := script.sessionStatus
			if status == 0 {
				status = http.StatusOK
			}
			body := script.sessionBody
			if body == "" {
				body = probeSessionBody
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(status)
			_, _ = writer.Write([]byte(body))
		case "/ws/mgw/":
			if script.handshakeFail != 0 {
				writer.WriteHeader(script.handshakeFail)
				return
			}
			if request.URL.Query().Get("uid") != probeTestUserID {
				t.Errorf("gateway uid query = %q, want %q", request.URL.Query().Get("uid"), probeTestUserID)
			}
			if !strings.Contains(request.Header.Get("Cookie"), "sso=token-1") || !strings.Contains(request.Header.Get("Cookie"), "x-userid="+probeTestUserID) {
				t.Errorf("gateway cookie = %q", request.Header.Get("Cookie"))
			}
			if request.Header.Get("Origin") != "http://"+request.Host {
				t.Errorf("gateway origin = %q", request.Header.Get("Origin"))
			}
			connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer connection.Close()
			var create map[string]any
			if err := connection.ReadJSON(&create); err != nil {
				t.Errorf("read session.create: %v", err)
				return
			}
			createEvent := create["event"].(map[string]any)
			createSession := createEvent["session"].(map[string]any)
			if model, _ := createSession["model"].(string); model != "fast" {
				t.Errorf("probe session model = %q, want fast", model)
			}
			xGrok := createSession["x_grok"].(map[string]any)
			if temporary, _ := xGrok["is_temporary"].(bool); !temporary {
				t.Errorf("probe session must be temporary, x_grok=%#v", xGrok)
			}
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "session.created", "client_event_id": createEvent["event_id"]}})
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}}})
			var itemMsg map[string]any
			if err := connection.ReadJSON(&itemMsg); err != nil {
				t.Errorf("read conversation.item.create: %v", err)
				return
			}
			itemEvent := itemMsg["event"].(map[string]any)
			if itemEvent["type"] != "conversation.item.create" {
				t.Fatalf("first turn event = %#v, want conversation.item.create", itemEvent)
			}
			item := itemEvent["item"].(map[string]any)
			chunks := item["x_grok"].(map[string]any)["input_chunks"].([]any)
			if prompt, _ := chunks[0].(map[string]any)["text"].(map[string]any)["text"].(string); prompt != probeDefaultPrompt {
				t.Errorf("probe prompt = %q, want %q", prompt, probeDefaultPrompt)
			}
			var respMsg map[string]any
			if err := connection.ReadJSON(&respMsg); err != nil {
				t.Errorf("read response.create: %v", err)
				return
			}
			respEvent := respMsg["event"].(map[string]any)
			if respEvent["type"] != "response.create" {
				t.Fatalf("second turn event = %#v, want response.create", respEvent)
			}
			if _, ok := respEvent["item"]; ok {
				t.Fatalf("response.create must not inline the user item: %#v", respEvent)
			}
			if script.delay > 0 {
				time.Sleep(script.delay)
			}
			for _, chunk := range script.chunks {
				_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": chunk})
			}
		default:
			fhttp.NotFound(writer, request)
		}
	}))
}

func probeChecker(server *fhttptest.Server) *SSOProbeChecker {
	checker := NewSSOProbeChecker(10 * time.Second)
	checker.baseURL = server.URL
	return checker
}

func chunkEvent(channel, text string) map[string]any {
	return map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": text, "channel": channel}}}
}

// A healthy account streams the notetaker header before the answer: clean.
func TestSSOProbeNotetakerHeaderIsClean(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelNotetakerHeader, "Thinking about your request"),
		chunkEvent(channelAssistantText, "answer"),
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), " token-1 ")
	if result.Verdict != VerdictClean {
		t.Fatalf("notetaker header = %#v, want clean", result)
	}
	if !strings.Contains(result.BotFlagDetails, "notetaker=1") {
		t.Fatalf("details should carry the probe summary, got %q", result.BotFlagDetails)
	}
}

// A reasoning chunk also proves the account is healthy.
func TestSSOProbeReasoningChunkIsClean(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelReasoning, "hmm"),
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictClean {
		t.Fatalf("reasoning chunk = %#v, want clean", result)
	}
	if !strings.Contains(result.BotFlagDetails, "reasoning=1") {
		t.Fatalf("details = %q", result.BotFlagDetails)
	}
}

// The live Web gateway treats CHANNEL_ANALYSIS as thinking (see
// appendGatewayDelta). The probe must too: otherwise a healthy UNIFIED
// stream that puts reasoning on ANALYSIS is classified denied the moment
// CHANNEL_ASSISTANT_RESPONSE arrives.
func TestSSOProbeAnalysisChannelIsClean(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent("CHANNEL_ANALYSIS", "thought"),
		chunkEvent(channelAssistantText, "OK."),
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictClean {
		t.Fatalf("CHANNEL_ANALYSIS then answer = %#v, want clean", result)
	}
}

// Channel names are matched case-insensitively like the live gateway.
func TestSSOProbeNotetakerChannelIsCaseInsensitive(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent("channel_assistant_notetaker_header", "Thinking about your request"),
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictClean {
		t.Fatalf("lowercase notetaker = %#v, want clean", result)
	}
}

// Bare answer with no thinking is the denied signature, but Check()
// without a fresh clean witness must suppress it.
func TestSSOProbeAnswerWithoutThinkingIsDenied(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelAssistantText, "OK."),
	}})
	defer server.Close()
	checker := probeChecker(server)
	result := checker.Check(context.Background(), "token-1")
	if result.Verdict != VerdictError || !result.Suppressed {
		t.Fatalf("bare answer without witness = %#v, want suppressed error", result)
	}
	if !strings.Contains(result.BotFlagDetails, "answer_without_thinking") || !strings.Contains(result.BotFlagDetails, "answer=\"OK.\"") {
		t.Fatalf("details = %q", result.BotFlagDetails)
	}
	if !strings.Contains(result.BotFlagDetails, "vitality=1/0") {
		t.Fatalf("details should carry the fresh vitality window, got %q", result.BotFlagDetails)
	}
	checker.vitality.recordAt(true, time.Now())
	trusted := checker.Check(context.Background(), "token-1")
	if trusted.Verdict != VerdictDenied {
		t.Fatalf("same answer after clean witness = %#v, want denied", trusted)
	}
}

// Stream errors are inconclusive: a rate-limited probe must never read as
// risk (denied) or healthy (clean).
func TestSSOProbeStreamErrorIsInconclusive(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		{"type": "response.grok.output", "output": map[string]any{"stream_error": map[string]any{"code": "rate_limited"}}},
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError {
		t.Fatalf("stream error = %#v, want error", result)
	}
	if !strings.Contains(result.Error, "rate_limited") {
		t.Fatalf("error = %q", result.Error)
	}
}

// A gateway error event is likewise inconclusive.
func TestSSOProbeGatewayErrorEventIsInconclusive(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		{"type": "error", "error": map[string]any{"message": "boom"}},
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError {
		t.Fatalf("gateway error = %#v, want error", result)
	}
}

// response.done before any decidable chunk stays inconclusive.
func TestSSOProbeDoneWithoutDecisionIsInconclusive(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		{"type": "response.done", "response": map[string]any{"status": "completed"}},
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError {
		t.Fatalf("silent done = %#v, want error", result)
	}
}

// An unauthenticated session is a credential problem, never a risk verdict.
func TestSSOProbeUnauthenticatedSessionIsError(t *testing.T) {
	server := probeServer(t, &probeServerScript{sessionBody: "{\"status\":\"unauthenticated\"}"})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError || !strings.Contains(result.Error, "unauthenticated") {
		t.Fatalf("unauthenticated = %#v, want error mentioning unauthenticated", result)
	}
}

// Session endpoint without a userId cannot dial the gateway: error.
func TestSSOProbeSessionWithoutUserIDIsError(t *testing.T) {
	server := probeServer(t, &probeServerScript{sessionBody: "{\"session\":{}}"})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError || !strings.Contains(result.Error, "userId") {
		t.Fatalf("missing userId = %#v", result)
	}
}

// A non-101 gateway handshake surfaces its HTTP status for the transient
// retry classification.
func TestSSOProbeForbiddenHandshakeIsErrorWithStatus(t *testing.T) {
	server := probeServer(t, &probeServerScript{handshakeFail: http.StatusForbidden})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError {
		t.Fatalf("403 handshake = %#v, want error", result)
	}
	if result.HTTPStatus != http.StatusForbidden {
		t.Fatalf("HTTPStatus = %d, want 403", result.HTTPStatus)
	}
}

func TestSSOProbeEmptyTokenIsError(t *testing.T) {
	result := NewSSOProbeChecker(time.Second).Check(context.Background(), "   ")
	if result.Verdict != VerdictError || result.Error != "empty sso token" {
		t.Fatalf("empty token = %#v", result)
	}
}

func TestNewSSOProbeCheckerDefaults(t *testing.T) {
	checker := NewSSOProbeChecker(0)
	if checker.Timeout != 45*time.Second {
		t.Fatalf("default timeout = %v, want 45s", checker.Timeout)
	}
	if checker.Model != "fast" || checker.Prompt != probeDefaultPrompt {
		t.Fatalf("defaults model=%q prompt=%q", checker.Model, checker.Prompt)
	}
}

// The channel-vocabulary / probe-path breaker: a denied with zero fresh
// clean witnesses is untrusted on the first sample.
func TestSSOProbeBreakerSuppressesDeniedAfterAllDeniedWindow(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelAssistantText, "OK."),
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError || !result.Suppressed {
		t.Fatalf("first denial = %#v, want suppressed error", result)
	}
	if !strings.Contains(result.Error, "suspect") {
		t.Fatalf("error = %q", result.Error)
	}
}

// A clean witness heals the breaker: denied works again afterwards.
func TestSSOProbeBreakerHealsOnCleanWitness(t *testing.T) {
	script := &probeServerScript{chunks: []map[string]any{chunkEvent(channelAssistantText, "OK.")}}
	server := probeServer(t, script)
	defer server.Close()
	checker := probeChecker(server)
	if result := checker.Check(context.Background(), "token-1"); result.Verdict != VerdictError || !result.Suppressed {
		t.Fatalf("first denial = %#v, want suppressed error", result)
	}
	script.chunks = []map[string]any{chunkEvent(channelNotetakerHeader, "Thinking"), chunkEvent(channelAssistantText, "OK.")}
	if result := checker.Check(context.Background(), "token-1"); result.Verdict != VerdictClean {
		t.Fatalf("healthy probe after breaker = %#v, want clean", result.Verdict)
	}
	script.chunks = []map[string]any{chunkEvent(channelAssistantText, "OK.")}
	if result := checker.Check(context.Background(), "token-1"); result.Verdict != VerdictDenied {
		t.Fatalf("denied after clean witness = %#v, want denied", result.Verdict)
	}
}

// A clean older than probeVitalityFresh must not keep the breaker off:
// that is the dirty-IP storm after an earlier clean-IP period.
func TestSSOProbeBreakerIgnoresStaleCleanWitness(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelAssistantText, "OK."),
	}})
	defer server.Close()
	checker := probeChecker(server)
	checker.vitality.recordAt(true, time.Now().Add(-probeVitalityFresh-time.Minute))
	result := checker.Check(context.Background(), "token-1")
	if result.Verdict != VerdictError || !result.Suppressed {
		t.Fatalf("stale clean must not witness: %#v suppressed=%v", result.Verdict, result.Suppressed)
	}
}

// A still-fresh clean continues to witness the current path, so denials
// stay account-level (the mixed-pool case).
func TestSSOProbeBreakerFreshCleanStillWitnesses(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelAssistantText, "OK."),
	}})
	defer server.Close()
	checker := probeChecker(server)
	checker.vitality.recordAt(true, time.Now().Add(-probeVitalityFresh/2))
	for i := 0; i < 3; i++ {
		if result := checker.Check(context.Background(), "token-1"); result.Verdict != VerdictDenied {
			t.Fatalf("denial %d = %#v, want denied (fresh clean must witness)", i+1, result.Verdict)
		}
	}
}

// Error verdicts never enter the breaker window (unreachable upstream must
// neither trip nor heal it).
func TestSSOProbeBreakerIgnoresErrorVerdicts(t *testing.T) {
	server := probeServer(t, &probeServerScript{handshakeFail: http.StatusForbidden})
	defer server.Close()
	checker := probeChecker(server)
	for i := 0; i < probeVitalityWindow+8; i++ {
		if result := checker.Check(context.Background(), "token-1"); result.Verdict != VerdictError {
			t.Fatalf("error probe %d = %#v", i, result.Verdict)
		}
	}
	server2 := probeServer(t, &probeServerScript{chunks: []map[string]any{chunkEvent(channelAssistantText, "OK.")}})
	defer server2.Close()
	checker.baseURL = server2.URL
	result := checker.Check(context.Background(), "token-1")
	if result.Verdict != VerdictError || !result.Suppressed {
		t.Fatalf("denial after errors = %#v, want suppressed (errors are not a clean witness)", result)
	}
}

// The probe verdicts must ride the shared Result shape the risk service
// persists (verdict vocabulary identical to the homepage checker).
func TestSSOProbeVerdictJSONRoundTrip(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelAssistantText, "hi"),
	}})
	defer server.Close()
	checker := probeChecker(server)
	checker.vitality.recordAt(true, time.Now())
	result := checker.Check(context.Background(), "token-1")
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "\"Verdict\":\"denied\"") {
		t.Fatalf("marshalled verdict missing denied: %s", encoded)
	}
}

// The live Web gateway waits for session.created AND conversation.attached,
// then split-sends conversation.item.create + an empty response.create.
// Inlining the user turn into response.create on the first attached event
// is a different mgw dialect and is what production used to miss thinking.
func TestSSOProbeTurnMatchesGatewaySplitSend(t *testing.T) {
	var mu sync.Mutex
	var clientEvents []string
	attachedFirst := make(chan struct{})
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		switch request.URL.Path {
		case "/api/auth/session":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(probeSessionBody))
		case "/ws/mgw/":
			connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer connection.Close()
			var create map[string]any
			if err := connection.ReadJSON(&create); err != nil {
				t.Errorf("read session.create: %v", err)
				return
			}
			mu.Lock()
			clientEvents = append(clientEvents, create["event"].(map[string]any)["type"].(string))
			mu.Unlock()
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}}})
			close(attachedFirst)
			time.Sleep(150 * time.Millisecond)
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "session.created", "client_event_id": create["event"].(map[string]any)["event_id"]}})
			for i := 0; i < 2; i++ {
				var msg map[string]any
				if err := connection.ReadJSON(&msg); err != nil {
					t.Errorf("read turn %d: %v", i, err)
					return
				}
				mu.Lock()
				clientEvents = append(clientEvents, msg["event"].(map[string]any)["type"].(string))
				mu.Unlock()
			}
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": chunkEvent(channelNotetakerHeader, "Thinking")})
		default:
			fhttp.NotFound(writer, request)
		}
	}))
	defer server.Close()

	done := make(chan Result, 1)
	go func() { done <- probeChecker(server).Check(context.Background(), "token-1") }()
	select {
	case <-attachedFirst:
	case <-time.After(2 * time.Second):
		t.Fatal("server never reached conversation.attached")
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	early := append([]string(nil), clientEvents...)
	mu.Unlock()
	for _, eventType := range early {
		if eventType == "response.create" || eventType == "conversation.item.create" {
			t.Fatalf("sent %q before session.created; events=%v", eventType, early)
		}
	}
	result := <-done
	if result.Verdict != VerdictClean {
		t.Fatalf("split-send probe = %#v, want clean", result)
	}
	mu.Lock()
	got := append([]string(nil), clientEvents...)
	mu.Unlock()
	want := []string{"session.create", "conversation.item.create", "response.create"}
	if len(got) < 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("client events = %v, want %v", got, want)
	}
}
