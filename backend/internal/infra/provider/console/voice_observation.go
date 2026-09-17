package console

import (
	"encoding/json"
	"math"

	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type observedVoiceWebSocket struct {
	*egress.WebSocket
	path              string
	observe           func(provider.VoiceWebSocketObservation)
	channels          int
	completedChannels uint16
}

func (c *observedVoiceWebSocket) ReadMessage() (int, []byte, error) {
	typ, payload, err := c.WebSocket.ReadMessage()
	if err == nil && c.observe != nil {
		if observation, channel, ok := parseVoiceObservation(c.path, payload); ok {
			if c.path == "/stt" && observation.Completed {
				count := max(1, c.channels)
				index := 0
				if channel != nil {
					index = *channel
				} else if count > 1 {
					return typ, payload, err
				}
				if index < 0 || index >= count {
					return typ, payload, err
				}
				mask := uint16(1) << index
				if c.completedChannels&mask != 0 {
					return typ, payload, err
				}
				c.completedChannels |= mask
				observation.Completed = c.completedChannels == (uint16(1)<<count)-1
				observation.Partial = !observation.Completed
			}
			c.observe(observation)
		}
	}
	return typ, payload, err
}

// The adapter interprets only documented root protocol events. Nested user or
// tool payloads cannot manufacture completion or audio usage. Socket closure
// itself carries no generation success claim.
func parseVoiceObservation(path string, payload []byte) (provider.VoiceWebSocketObservation, *int, bool) {
	var event struct {
		Type         string   `json:"type"`
		Duration     *float64 `json:"duration"`
		ChannelIndex *int     `json:"channel_index"`
		Response     struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return provider.VoiceWebSocketObservation{}, nil, false
	}
	observation := provider.VoiceWebSocketObservation{}
	switch {
	case path == "/stt" && event.Type == "transcript.created", path == "/realtime" && event.Type == "response.created":
		observation.Started = true
	case path == "/stt" && event.Type == "transcript.done":
		observation.Completed = true
		if event.Duration != nil && *event.Duration >= 0 && !math.IsNaN(*event.Duration) && !math.IsInf(*event.Duration, 0) {
			observation.AudioDurationReported, observation.AudioDurationSeconds = true, *event.Duration
		}
	case path == "/realtime" && event.Type == "response.done":
		observation.Completed = event.Response.Status == "" || event.Response.Status == "completed"
		observation.Failed = !observation.Completed
	case event.Type == "error":
		observation.Failed = true
	default:
		return observation, nil, false
	}
	return observation, event.ChannelIndex, true
}
