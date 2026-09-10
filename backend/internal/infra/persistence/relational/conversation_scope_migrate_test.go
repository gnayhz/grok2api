package relational

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	reasoningreplay "github.com/chenyme/grok2api/backend/internal/application/history"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestConversationAccountScopeMigrationPreservesAllTurns(t *testing.T) {
	_, j, _ := journalFixture(t)
	r := newJournalReplay(j)
	ctx := context.Background()
	history := []any{map[string]any{"role": "user", "content": "one"}}
	for turn, key := range []string{"account-a", "account-a", "account-b", "account-b"} {
		body, _ := json.Marshal(map[string]any{"input": history})
		_, p, e := r.Prepare(ctx, "model", key, body)
		if e != nil {
			t.Fatal(e)
		}
		assistant := map[string]any{"role": "assistant", "content": string(rune('a' + turn))}
		response, _ := json.Marshal(map[string]any{"id": string(rune('a' + turn)), "status": "completed", "output": []any{journalCipherItem(t), assistant}})
		stream, commit, discard := p.Capture(io.NopCloser(bytes.NewReader(response)), false)
		if _, e = io.Copy(io.Discard, stream); e != nil {
			t.Fatal(e)
		}
		if e = commit(); e != nil {
			t.Fatal(e)
		}
		stream.Close()
		discard()
		history = append(history, assistant, map[string]any{"role": "user", "content": "next"})
	}
	body, _ := json.Marshal(map[string]any{"input": history})
	restored, p, e := r.Prepare(ctx, "model", "shared", body, historydomain.ReplayPreparation{LegacyKeys: []string{"account-a", "account-b"}})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Discard()
	if p.RestoredItems() != 4 || p.Outcome() != "append_ok" {
		t.Fatalf("restored=%d outcome=%s", p.RestoredItems(), p.Outcome())
	}
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	json.Unmarshal(restored, &request)
	var visible []json.RawMessage
	for _, item := range request.Input {
		_, reason, err := reasoningreplay.CanonicalHistoryItemHash(item)
		if err != nil {
			t.Fatal(err)
		}
		if !reason {
			visible = append(visible, item)
		}
	}
	actual, _ := json.Marshal(visible)
	expected, _ := json.Marshal(history)
	if !bytes.Equal(actual, expected) {
		t.Fatal("visible content changed")
	}
	p.Discard()
	// Repeat after restart; original account scopes cannot accept stale writers.
	r = newJournalReplay(j)
	_, next, e := r.Prepare(ctx, "model", "shared", body, historydomain.ReplayPreparation{LegacyKeys: []string{"account-a", "account-b"}})
	if e != nil {
		t.Fatal(e)
	}
	if next.RestoredItems() != 4 {
		t.Fatal("migration was not durable")
	}
	response := []byte(`{"id":"migration-continuation","status":"completed","output":[{"role":"assistant","content":"continued"}]}`)
	stream, commit, discard := next.Capture(io.NopCloser(bytes.NewReader(response)), false)
	if _, e = io.Copy(io.Discard, stream); e != nil {
		t.Fatal(e)
	}
	if e = commit(); e != nil {
		t.Fatal(e)
	}
	stream.Close()
	discard()
	var session conversationSessionModel
	if e = j.db.First(&session, "id = ?", journalScopeID(repository.JournalScope{Model: "model", Key: "shared", Normalizer: 1})).Error; e != nil {
		t.Fatal(e)
	}
	var head conversationTurnModel
	if e = j.db.First(&head, "id = ?", session.Head).Error; e != nil {
		t.Fatal(e)
	}
	if head.ResponseID != "migration-continuation" {
		t.Fatal("migrated head failed to advance")
	}
	_, _, e = r.Prepare(ctx, "model", "account-a", body)
	if !errors.Is(e, historydomain.ErrHistoryStale) {
		t.Fatalf("legacy writer not fenced: %v", e)
	}
	if _, e = j.Prune(ctx, time.Now().Add(48*time.Hour), 100); e != nil {
		t.Fatal(e)
	}
}

func TestConversationAccountMigrationWaitsForActiveWriter(t *testing.T) {
	_, j, _ := journalFixture(t)
	r := newJournalReplay(j)
	ctx := context.Background()
	body := []byte(`{"input":[{"role":"user","content":"one"}]}`)
	_, old, e := r.Prepare(ctx, "model", "old", body)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = r.Prepare(ctx, "model", "new", body, historydomain.ReplayPreparation{LegacyKeys: []string{"old"}})
	if !errors.Is(e, historydomain.ErrHistoryStale) {
		t.Fatalf("active writer migration: %v", e)
	}
	old.Discard()
	_, next, e := r.Prepare(ctx, "model", "new", body, historydomain.ReplayPreparation{LegacyKeys: []string{"old"}})
	if e != nil {
		t.Fatal(e)
	}
	next.Discard()
}
