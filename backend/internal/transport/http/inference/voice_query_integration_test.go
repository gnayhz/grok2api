package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	upstreamws "github.com/bogdanfinn/websocket"
	clientws "github.com/gorilla/websocket"
)

func TestVoiceSTTQueryReachesUpstreamIntegration(t *testing.T) {
	options := url.Values{"encoding": {"pcm"}, "sample_rate": {"48000"}, "interim_results": {"true"}, "endpointing": {"0"}, "language": {"zh-CN"}, "multichannel": {"true"}, "channels": {"2"}, "diarize": {"true"}, "keyterm": {"hello world", "宇宙 & +/%"}, "filler_words": {"true"}, "smart_turn": {"0.7"}, "smart_turn_timeout": {"3000"}, "vad_threshold": {"0"}}
	expected := url.Values{}
	for k, v := range options {
		expected[k] = v
	}
	expected.Set("model", "grok-stt")
	observed := make(chan url.Values, 1)
	var calls atomic.Int32
	upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
		observed <- r.URL.Query()
		c, err := (&upstreamws.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		_ = c.WriteMessage(upstreamws.TextMessage, []byte(`{"type":"transcript.done","channel_index":0,"duration":3.45}`))
		_ = c.WriteMessage(upstreamws.TextMessage, []byte(`{"type":"transcript.done","channel_index":1,"duration":3.45}`))
		_ = c.WriteControl(upstreamws.CloseMessage, upstreamws.FormatCloseMessage(1000, "done"), time.Now().Add(time.Second))
	})
	defer upstream.Close()
	fx := newVoiceCompletionFixture(t, upstream.URL, "grok-stt")
	server := httptest.NewServer(fx.router)
	defer server.Close()
	c, _, err := clientws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/stt?"+expected.Encode(), http.Header{"Authorization": []string{"Bearer " + fx.created.Secret}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
	if got := <-observed; !reflect.DeepEqual(got, expected) {
		t.Fatalf("audio configuration dropped: got=%v want=%v", got, expected)
	}
	record := waitVoiceAudit(t, fx.audits)
	if record.GenerationOutcome != "completed" || record.AudioDurationMS != 3450 || record.DeliveredEvents != 2 {
		t.Fatalf("multichannel facts: %+v", record)
	}
}

func TestVoiceQueryValidationDoesNotAttemptUpstreamIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, path, query, param string
		transport                bool
	}{
		{name: "format", path: "stt", query: "encoding=mp3", param: "encoding"},
		{name: "sample_rate", path: "stt", query: "sample_rate=17000", param: "sample_rate"},
		{name: "duplicate", path: "stt", query: "sample_rate=16000&sample_rate=48000", param: "sample_rate"},
		{name: "channels", path: "stt", query: "multichannel=true", param: "channels"},
		{name: "channels_range", path: "stt", query: "channels=9", param: "channels"},
		{name: "opus", path: "stt", query: "encoding=opus&multichannel=true&channels=2", param: "encoding"},
		{name: "timeout", path: "stt", query: "smart_turn_timeout=1", param: "smart_turn_timeout"},
		{name: "nan", path: "stt", query: "vad_threshold=NaN", param: "vad_threshold"},
		{name: "infinity", path: "stt", query: "smart_turn=Inf", param: "smart_turn"},
		{name: "range", path: "stt", query: "endpointing=5001", param: "endpointing"},
		{name: "boolean", path: "stt", query: "diarize=perhaps", param: "diarize"},
		{name: "empty_keyterm", path: "stt", query: "keyterm=", param: "keyterm"},
		{name: "long_keyterm", path: "stt", query: "keyterm=" + strings.Repeat("x", 51), param: "keyterm"},
		{name: "many_keyterms", path: "stt", query: strings.Repeat("keyterm=x&", 100) + "keyterm=y", param: "keyterm"},
		{name: "unknown", path: "stt", query: "unrecognized=true", param: "unrecognized"},
		{name: "sip_ownership", path: "realtime", query: "call_id=unowned", param: "call_id"},
		{name: "duplicate_model", path: "stt", query: "model=other", transport: true},
		{name: "malformed_escape", path: "stt", query: "encoding=%zz", transport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer upstream.Close()
			model := "grok-stt"
			if tc.path == "realtime" {
				model = "grok-voice-latest"
			}
			fx := newVoiceCompletionFixture(t, upstream.URL, model)
			server := httptest.NewServer(fx.router)
			defer server.Close()
			conn, response, err := clientws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/"+tc.path+"?model="+model+"&"+tc.query, http.Header{"Authorization": []string{"Bearer " + fx.created.Secret}})
			if conn != nil {
				_ = conn.Close()
			}
			if err == nil || response == nil || response.StatusCode != 400 {
				t.Fatalf("validation response=%v err=%v", response, err)
			}
			defer response.Body.Close()
			var payload struct{ Error struct{ Code, Param string } }
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Code != "invalid_request" || !tc.transport && payload.Error.Param != tc.param {
				t.Fatalf("validation payload=%+v", payload)
			}
			if calls.Load() != 0 {
				t.Fatalf("local validation submitted %d upstream calls", calls.Load())
			}
			if !tc.transport {
				record := waitVoiceAudit(t, fx.audits)
				if record.AccountID != nil || record.UpstreamStatusCode != 0 || record.GenerationOutcome != "not_started" || record.AdmissionOutcome != "not_admitted" || record.DeliveryOutcome != "not_started" || record.ErrorCode != "invalid_request" {
					t.Fatalf("local error became upstream/credential failure: %+v", record)
				}
			}
		})
	}
}

func TestVoiceSTTChannelCompletionIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, query, generation string
		events                  []string
		duration                int64
	}{
		{name: "missing_channel", query: "multichannel=true&channels=2", generation: "partial", events: []string{`{"type":"transcript.done","channel_index":0,"duration":3.45}`}, duration: 3450},
		{name: "duplicate_channel", query: "multichannel=true&channels=2", generation: "partial", events: []string{`{"type":"transcript.done","channel_index":0,"duration":3.45}`, `{"type":"transcript.done","channel_index":0,"duration":345}`}, duration: 3450},
		{name: "wrong_channel", query: "multichannel=true&channels=2", generation: "unconfirmed", events: []string{`{"type":"transcript.done","channel_index":2,"duration":3.45}`}},
		{name: "unidentified_channel", query: "multichannel=true&channels=2", generation: "unconfirmed", events: []string{`{"type":"transcript.done","duration":3.45}`}},
		{name: "out_of_order_channels", query: "multichannel=true&channels=2", generation: "completed", events: []string{`{"type":"transcript.done","channel_index":1,"duration":3.45}`, `{"type":"transcript.done","channel_index":0,"duration":3.45}`}, duration: 3450},
		{name: "partial_then_error", query: "multichannel=true&channels=2", generation: "partial", events: []string{`{"type":"transcript.done","channel_index":0,"duration":3.45}`, `{"type":"error","message":"other channel failed"}`}, duration: 3450},
		{name: "opus_mono", query: "encoding=opus&channels=1", generation: "completed", events: []string{`{"type":"transcript.done","duration":3.45}`}, duration: 3450},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				c, err := (&upstreamws.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				for _, event := range tc.events {
					if err := c.WriteMessage(upstreamws.TextMessage, []byte(event)); err != nil {
						t.Error(err)
						return
					}
				}
				_ = c.WriteControl(upstreamws.CloseMessage, upstreamws.FormatCloseMessage(1000, "done"), time.Now().Add(time.Second))
			})
			defer upstream.Close()
			fx := newVoiceCompletionFixture(t, upstream.URL, "grok-stt")
			server := httptest.NewServer(fx.router)
			defer server.Close()
			c, _, err := clientws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/stt?model=grok-stt&"+tc.query, http.Header{"Authorization": []string{"Bearer " + fx.created.Secret}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			for _, event := range tc.events {
				_, data, err := c.ReadMessage()
				if err != nil || string(data) != event {
					t.Fatalf("forwarded=%s error=%v", data, err)
				}
			}
			_, _, _ = c.ReadMessage()
			record := waitVoiceAudit(t, fx.audits)
			if record.GenerationOutcome != tc.generation || record.DeliveryOutcome != "completed" || record.AudioDurationMS != tc.duration {
				t.Fatalf("channel completion: %+v", record)
			}
			if tc.duration > 0 && record.EstimatedCostInUSDTicks != 1916667 {
				t.Fatalf("duration lost or duplicate channel double charged: %+v", record)
			}
			if tc.duration == 0 && record.UsageSource != "none" {
				t.Fatalf("unexpected channel manufactured usage: %+v", record)
			}
		})
	}
}
