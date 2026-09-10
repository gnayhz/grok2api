package inference

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
)

type voiceRelayFixture struct {
	read  func() (int, []byte, error)
	write func(int, []byte) error
	close func()
}

func (c voiceRelayFixture) ReadMessage() (int, []byte, error)       { return c.read() }
func (c voiceRelayFixture) WriteMessage(typ int, data []byte) error { return c.write(typ, data) }
func (c voiceRelayFixture) Close() error {
	if c.close != nil {
		c.close()
	}
	return nil
}

func TestVoiceRelayJoinsLateUpstreamFactsAfterClientClose(t *testing.T) {
	clientDone, producerRead, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	upstream := voiceRelayFixture{read: func() (int, []byte, error) {
		close(producerRead)
		<-clientDone
		<-release
		return 1, []byte("late final frame"), nil
	}, write: func(int, []byte) error { return nil }, close: func() { once.Do(func() { close(clientDone) }) }}
	client := voiceRelayFixture{read: func() (int, []byte, error) { <-producerRead; return 0, nil, io.EOF }, write: func(int, []byte) error { return io.ErrClosedPipe }}
	result := make(chan gateway.VoiceWebSocketOutcome, 1)
	go func() { result <- relayVoiceWebSocket(context.Background(), upstream, client) }()
	<-clientDone
	select {
	case got := <-result:
		t.Fatalf("finalized before last upstream observation: %+v", got)
	default:
	}
	close(release)
	select {
	case got := <-result:
		if got.DeliveredBytes != 0 || got.DeliveredEvents != 0 {
			t.Fatalf("failed write counted as delivered: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not join after source exit")
	}
}

func TestVoiceRelayCancellationJoinsBothPumpsAndRetainsDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var once sync.Once
	var upstreamReads int
	upstream := voiceRelayFixture{read: func() (int, []byte, error) {
		upstreamReads++
		if upstreamReads == 1 {
			return 1, []byte("answer"), nil
		}
		<-done
		return 0, nil, io.ErrClosedPipe
	}, write: func(int, []byte) error { return nil }, close: func() { once.Do(func() { close(done) }) }}
	client := voiceRelayFixture{read: func() (int, []byte, error) { <-done; return 0, nil, io.ErrClosedPipe }, write: func(int, []byte) error { cancel(); return nil }}
	got := relayVoiceWebSocket(ctx, upstream, client)
	if got.ErrorCode != "request_canceled" || got.UpstreamFailed || got.DeliveredBytes != 6 || got.DeliveredEvents != 1 {
		t.Fatalf("cancel lost actual delivery: %+v", got)
	}
}
