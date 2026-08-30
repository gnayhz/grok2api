package gateway

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentPeekStreamsKeepReplayIsolated: the guard's per-request state
// (scan scratch, held buffer, read pump, replay reader) must be fully
// isolated across concurrent streams. N pipes are fed interleaved with
// unique markers and odd chunk splits; every replay must contain exactly its
// own marker. Shared-state regressions (e.g. a scanner buffer or pump leaking
// across requests) surface as cross-talk or lost frames.
func TestConcurrentPeekStreamsKeepReplayIsolated(t *testing.T) {
	t.Parallel()
	const streams = 32
	var wg sync.WaitGroup
	errCh := make(chan error, streams)
	for s := 0; s < streams; s++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			marker := fmt.Sprintf("iso-%04d-%s", idx, strings.Repeat("word ", 10))
			body := ": grok2api-reasoning-start\n\n" +
				"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"" + marker + "\"}}]}\n\n" +
				"data: {\"usage\":{\"completion_tokens\":30,\"completion_tokens_details\":{\"reasoning_tokens\":12}}}\n\n" +
				"data: [DONE]\n\n"
			reader, writer := io.Pipe()
			writerDone := make(chan struct{})
			go func() {
				defer close(writerDone)
				defer writer.Close()
				// Feed with a per-stream odd chunk size to force varied frame splits.
				chunk := 7 + idx%5
				for len(body) > 0 {
					n := chunk
					if n > len(body) {
						n = len(body)
					}
					if _, err := writer.Write([]byte(body[:n])); err != nil {
						return
					}
					body = body[n:]
				}
			}()
			replay, verdict, _, err := peekQualityStream(context.Background(), reader, qualityProtocolChat,
				QualityRetryRuntime{})
			if err != nil {
				errCh <- fmt.Errorf("stream %d: %w", idx, err)
				return
			}
			replayed, readErr := io.ReadAll(replay)
			if readErr != nil {
				errCh <- fmt.Errorf("stream %d replay: %w", idx, readErr)
				return
			}
			text := string(replayed)
			want := fmt.Sprintf("iso-%04d", idx)
			if !strings.Contains(text, want) {
				errCh <- fmt.Errorf("stream %d replay lost its marker", idx)
			}
			if strings.Count(text, "iso-") != 1 {
				errCh <- fmt.Errorf("stream %d replay carries %d markers (cross-talk)", idx, strings.Count(text, "iso-"))
			}
			if verdict != QualityDeliver {
				errCh <- fmt.Errorf("stream %d verdict = %s, want deliver", idx, verdict)
			}
			select {
			case <-writerDone:
			case <-time.After(2 * time.Second):
				errCh <- fmt.Errorf("stream %d writer goroutine stuck", idx)
			}
		}(s)
	}
	wg.Wait()
	close(errCh)
	for streamErr := range errCh {
		t.Error(streamErr)
	}
}
