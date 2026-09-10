package web

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

func TestWebResourcesRejectBeforeSemanticAllocation(t *testing.T) {
	pool := responsebuffer.NewPool(100 << 10)
	resources := newWebResponseResources(responsebuffer.WithContext(context.Background(), pool.Request(100<<10)))
	parsed := parsedChat{resources: resources}
	raw := `{"result":{"response":{"token":"` + strings.Repeat("x", 16<<10) + `"}}}`
	err := consumeUpstreamInto(strings.NewReader(raw), &parsed, nil)
	if !errors.Is(err, responsebuffer.ErrExhausted) || parsed.Text.Len() != 0 {
		t.Fatalf("decoded before reservation: err=%v text=%d", err, parsed.Text.Len())
	}
	resources.Close()
	if used := pool.Snapshot().Used; used != 0 {
		t.Fatalf("leaked %d", used)
	}
}

type closedWebSource struct {
	io.Reader
	done chan struct{}
}

func (s *closedWebSource) Close() error { close(s.done); return nil }

func TestWebResourcesReleasedAfterConcurrentCompletionAndCancel(t *testing.T) {
	pool := responsebuffer.NewPool(64 << 20)
	const concurrency = 16
	for wave := 0; wave < 12; wave++ {
		var wg sync.WaitGroup
		for worker := 0; worker < concurrency; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx := responsebuffer.WithContext(context.Background(), pool.Request(8<<20))
				source := &closedWebSource{Reader: strings.NewReader(strings.Repeat(`{"result":{"response":{"token":"answer text fragment ","messageTag":"final"}}}`, 128)), done: make(chan struct{})}
				stream := new(Adapter).streamOpenAIResponse(ctx, source, new(egress.Lease), account.Credential{}, "resp_budget", "grok", "responses", "prompt", nil, toolConfiguration{}, true, conversation.ResponseOptions{}, nil, "")
				if worker%2 == 0 {
					_, _ = io.CopyN(io.Discard, stream, 256)
				} else {
					if _, err := io.Copy(io.Discard, stream); err != nil {
						t.Error(err)
					}
				}
				stream.Close()
				select {
				case <-source.done:
				case <-time.After(2 * time.Second):
					t.Error("source not closed")
				}
			}()
		}
		wg.Wait()
		if used := pool.Snapshot().Used; used != 0 {
			t.Fatalf("wave=%d leaked=%d", wave, used)
		}
	}
	t.Logf("requests=%d concurrent=%d peak=%d retained=%d", 12*concurrency, concurrency, pool.Snapshot().Peak, pool.Snapshot().Used)
}
