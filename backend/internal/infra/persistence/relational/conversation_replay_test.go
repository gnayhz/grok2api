package relational

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	reasoningreplay "github.com/chenyme/grok2api/backend/internal/application/history"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func newJournalReplay(j *ConversationJournal) *reasoningreplay.ReasoningReplay {
	r := reasoningreplay.New(memory.NewReasoningReplayStore(1000), reasoningreplay.Config{Enabled: true, TTL: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.UseJournal(j, 24*time.Hour, time.Hour)
	return r
}
func journalCipherItem(t *testing.T) map[string]any {
	t.Helper()
	raw := make([]byte, 1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"type": "reasoning", "encrypted_content": base64.RawStdEncoding.EncodeToString(raw), "summary": []any{}}
}
func TestConversationReplayTwoHundredTurnsWithColdMemory(t *testing.T) {
	_, journal, _ := journalFixture(t)
	ctx := context.Background()
	history := []any{}
	for n := 0; n < 200; n++ {
		// A brand new L1 on every request models arbitrary eviction and replica
		// routing. All opaque content must still come from durable history.
		replay := newJournalReplay(journal)
		history = append(history, map[string]any{"role": "user", "content": fmt.Sprintf("question %d", n)})
		body, _ := json.Marshal(map[string]any{"input": history, "instructions": "stable system"})
		restored, p, err := replay.Prepare(ctx, "model", "session", body)
		if err != nil {
			t.Fatal(err)
		}
		if got := bytes.Count(restored, []byte(`"encrypted_content"`)); got != n {
			t.Fatalf("turn %d restored %d", n, got)
		}
		assistant := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("answer %d", n)}}}
		response, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("r%d", n), "status": "completed", "output": []any{journalCipherItem(t), assistant}})
		stream := n%2 == 0
		payload := response
		if stream {
			payload = []byte("data: {\"type\":\"response.completed\",\"response\":" + string(response) + "}\n\n")
		}
		captured, commit, discard := p.Capture(io.NopCloser(bytes.NewReader(payload)), stream)
		if _, err = io.Copy(io.Discard, captured); err != nil {
			t.Fatal(err)
		}
		_ = captured.Close()
		if err = commit(); err != nil {
			t.Fatalf("turn %d: %v", n, err)
		}
		discard()
		history = append(history, assistant)
	}
}
func TestConversationReplayCaptureDoesNotCommitOnCloseOrAfterReset(t *testing.T) {
	_, journal, _ := journalFixture(t)
	replay := newJournalReplay(journal)
	ctx := context.Background()
	body := []byte(`{"input":[{"role":"user","content":"question"}]}`)
	_, p, err := replay.Prepare(ctx, "model", "session", body)
	if err != nil {
		t.Fatal(err)
	}
	response := `{"id":"old","status":"completed","output":[{"role":"assistant","content":"answer"}]}`
	capture, commit, discard := p.Capture(io.NopCloser(strings.NewReader(response)), false)
	defer discard()
	_, _ = io.Copy(io.Discard, capture)
	_ = capture.Close()
	var count int64
	journal.db.Model(&conversationTurnModel{}).Count(&count)
	if count != 0 {
		t.Fatal("close committed output")
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := commit(); !errors.Is(err, historydomain.ErrHistoryCommit) {
		t.Fatalf("old completion accepted: %v", err)
	}
}
