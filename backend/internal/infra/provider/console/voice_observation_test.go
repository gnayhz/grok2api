package console

import "testing"

func TestVoiceProtocolObservationsPreserveKnownAndUnknownUsage(t *testing.T) {
	for _, tt := range []struct {
		path, payload                   string
		ok, completed, failed, reported bool
		duration                        float64
	}{
		{"/stt", `{"type":"transcript.done","duration":3.45}`, true, true, false, true, 3.45},
		{"/stt", `{"type":"transcript.done","duration":0}`, true, true, false, true, 0},
		{"/stt", `{"type":"transcript.done"}`, true, true, false, false, 0},
		{"/stt", `{"type":"transcript.done","duration":-1}`, true, true, false, false, 0},
		{"/stt", `{"type":"transcript.partial","duration":3.45}`, false, false, false, false, 0},
		{"/stt", `{"payload":{"type":"transcript.done","duration":3.45}}`, false, false, false, false, 0},
		{"/stt", `{"type":"transcript.done","duration":"3.45"}`, false, false, false, false, 0},
		{"/realtime", `{"type":"response.done","response":{"status":"completed"}}`, true, true, false, false, 0},
		{"/realtime", `{"type":"response.done","response":{"status":"failed"}}`, true, false, true, false, 0},
		{"/realtime", `{"type":"error","error":{"message":"recoverable"}}`, true, false, true, false, 0},
		{"/realtime", `{"type":"transcript.done","duration":3.45}`, false, false, false, false, 0},
	} {
		got, _, ok := parseVoiceObservation(tt.path, []byte(tt.payload))
		if ok != tt.ok || got.Completed != tt.completed || got.Failed != tt.failed || got.AudioDurationReported != tt.reported || got.AudioDurationSeconds != tt.duration {
			t.Fatalf("%s %s: %+v ok=%t", tt.path, tt.payload, got, ok)
		}
	}
}
