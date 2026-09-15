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

func TestConversationReplayPreservesProviderOpaqueEncoding(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	existing := make([]byte, 256)
	for i := range existing {
		existing[i] = byte(i)
	}
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, journal := identityJournalFixture(t, dialect)
			for _, tc := range []struct{ name, opaque string }{
				{"short", base64.RawStdEncoding.EncodeToString(raw)},
				{"padded", base64.StdEncoding.EncodeToString(raw)},
				{"url_encoding", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{251, 255}, 32))},
				{"provider_defined", "synthetic:opaque:v2:example"},
				{"low_entropy", base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0}, 128))},
				{"existing_encoding", base64.RawStdEncoding.EncodeToString(existing)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := t.Context()
					key := fmt.Sprintf("synthetic/%s/%d", t.Name(), time.Now().UnixNano())
					replay := newJournalReplay(journal)
					_, prepared, err := replay.Prepare(ctx, "synthetic-model", key, []byte(`{"input":[{"role":"user","content":"synthetic question"}]}`))
					if err != nil {
						t.Fatal(err)
					}
					opaque := map[string]any{"type": "reasoning", "id": "synthetic-reasoning", "status": "completed", "encrypted_content": tc.opaque, "summary": []any{}}
					commitIdentityTurn(t, prepared, "synthetic-response", "synthetic answer", opaque)
					// A new service and journal have no cached output. Restoration must
					// read the encrypted SQL row with exactly the provider's bytes.
					cold := newJournalReplay(NewConversationJournal(db, journal.cipher, 64<<20))
					input := []any{map[string]any{"role": "user", "content": "synthetic question"}, map[string]any{"role": "assistant", "content": "synthetic answer"}, map[string]any{"role": "user", "content": "synthetic next"}}
					body, _ := json.Marshal(map[string]any{"input": input})
					restored, next, err := cold.Prepare(ctx, "synthetic-model", key, body)
					if err != nil {
						t.Fatal(err)
					}
					if next.Outcome() != "append_ok" || next.RestoredItems() != 1 {
						t.Fatalf("lost durable reasoning: %s/%d", next.Outcome(), next.RestoredItems())
					}
					next.Discard()
					var decoded struct{ Input []map[string]json.RawMessage }
					if err := json.Unmarshal(restored, &decoded); err != nil || len(decoded.Input) != 4 {
						t.Fatal("invalid restored input")
					}
					var actual string
					if err := json.Unmarshal(decoded.Input[1]["encrypted_content"], &actual); err != nil || actual != tc.opaque {
						t.Fatal("opaque encoding changed during persistence or restoration")
					}
					if _, exists := decoded.Input[1]["status"]; exists {
						t.Fatal("output-only status was replayed")
					}
					// Clients may carry the same opaque item on the next request.
					// It must match the stored boundary without duplication.
					_, carried, err := cold.Prepare(ctx, "synthetic-model", key, restored)
					if err != nil {
						t.Fatal(err)
					}
					if carried.RestoredItems() != 0 || carried.Outcome() != "append_ok" {
						t.Fatal("client-carried opaque no longer matches its durable boundary")
					}
					carried.Discard()
					decoded.Input[1]["encrypted_content"] = json.RawMessage(`"synthetic-conflicting-opaque"`)
					conflict, _ := json.Marshal(map[string]any{"input": decoded.Input})
					if _, rejected, err := cold.Prepare(ctx, "synthetic-model", key, conflict); !errors.Is(err, historydomain.ErrHistoryAmbiguous) {
						historydomain.Discard(rejected)
						t.Fatalf("conflicting client opaque accepted: %v", err)
					}
					_, isolated, err := cold.Prepare(ctx, "synthetic-model", key+"/other", body)
					if err != nil {
						t.Fatal(err)
					}
					if isolated.RestoredItems() != 0 {
						t.Fatal("opaque history crossed scopes")
					}
					isolated.Discard()
				})
			}
		})
	}
}
