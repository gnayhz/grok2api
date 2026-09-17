package console

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"testing"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestVoiceWebSocketPreparationOwnsOptionsWithoutNetwork(t *testing.T) {
	adapter := &Adapter{}
	original := url.Values{"encoding": {"alaw"}, "sample_rate": {"8000"}, "diarize": {"1"}, "interim_results": {"FALSE"}, "keyterm": {"hello world", "宇宙"}, "smart_turn": {"0.0"}, "smart_turn_timeout": {"1"}, "channels": {"8"}}
	prepared, err := adapter.PrepareVoiceWebSocket(provider.VoiceWebSocketRequest{Path: "/stt", Model: "authorized-model", Query: original})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Model != "authorized-model" || prepared.Query.Get("diarize") != "true" || prepared.Query.Get("interim_results") != "false" || prepared.Query.Get("smart_turn") != "0" {
		t.Fatalf("prepared=%+v", prepared)
	}
	original["keyterm"][0] = "changed after preparation"
	original.Set("sample_rate", "48000")
	if !reflect.DeepEqual(prepared.Query["keyterm"], []string{"hello world", "宇宙"}) || prepared.Query.Get("sample_rate") != "8000" {
		t.Fatalf("caller mutation changed prepared protocol: %+v", prepared)
	}
	// The direct adapter entry also enforces the same rule before decrypt/dial.
	// An empty Adapter has no cipher or network; invalid input must not use them.
	for _, query := range []url.Values{{"model": {"unapproved"}}, {"encoding": {"opus"}, "channels": {"2"}}} {
		_, _, err := adapter.DialVoiceWebSocket(context.Background(), provider.VoiceWebSocketRequest{Path: "/stt", Model: "authorized-model", Query: query})
		var validation *inferencedomain.RequestValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("local validation did not precede credential/network: %v", err)
		}
	}
}
