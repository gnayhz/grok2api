package relational

import (
	"context"
	"encoding/json"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"os"
	"path/filepath"
	"testing"

	reasoningreplay "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Opt-in regression for the ten-turn conversation split by an exhausted account.
// The copy renames opaque scope IDs only; encrypted items and original node IDs
// remain unchanged. No user content is logged or sent to any upstream endpoint.
func TestConversationAccountMigrationSnapshot(t *testing.T) {
	path := os.Getenv("HISTORY_ACCOUNT_MIGRATION_SNAPSHOT")
	if path == "" {
		t.Skip("snapshot opt-in")
	}
	cfg, e := config.Load(filepath.Join(path, "config.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(path, "backend.db"))
	if e != nil {
		t.Fatal(e)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if e = os.WriteFile(copyPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	db, e := OpenSQLite(context.Background(), copyPath)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.db.AutoMigrate(&conversationSessionModel{}); e != nil {
		t.Fatal(e)
	}
	cipher, e := security.NewVersionedCipher(cfg.Secrets.CredentialEncryptionKey, cfg.Secrets.LegacyEncryptionKeys)
	if e != nil {
		t.Fatal(e)
	}
	j := NewConversationJournal(db, cipher, 0)
	var nodes []conversationTurnModel
	if e = db.db.Where("created_at >= ? AND created_at < ?", "2026-09-07 12:47:00", "2026-09-07 12:52:00").Order("created_at, id").Find(&nodes).Error; e != nil {
		t.Fatal(e)
	}
	if len(nodes) != 10 {
		t.Fatalf("fixture nodes=%d want10", len(nodes))
	}
	model := "grok-4.6"
	aliases := map[string]string{}
	var keys []string
	for _, n := range nodes {
		if _, ok := aliases[n.Session]; !ok {
			key := "snapshot-account-" + n.Session
			keys = append(keys, key)
			aliases[n.Session] = journalScopeID(repository.JournalScope{Model: model, Key: key, Normalizer: 1})
		}
	}
	if len(aliases) != 2 {
		t.Fatalf("fixture accounts=%d", len(aliases))
	}
	chain, e := j.loadChain(db.db, nodes[9].Session, nodes[9].Generation, nodes[9])
	if e != nil {
		t.Fatal(e)
	}
	var visible []json.RawMessage
	for _, turn := range chain {
		for _, item := range append(turn.Input, turn.Output...) {
			_, reason, e := reasoningreplay.CanonicalHistoryItemHash(item)
			if e != nil {
				t.Fatal(e)
			}
			if !reason {
				visible = append(visible, item)
			}
		}
	}
	if len(visible) != 20 {
		t.Fatalf("visible items=%d", len(visible))
	}
	visible = append(visible, json.RawMessage(`{"role":"user","content":"snapshot continuation"}`))
	for old, next := range aliases {
		if e = db.db.Model(&conversationSessionModel{}).Where("id = ?", old).Update("id", next).Error; e != nil {
			t.Fatal(e)
		}
		if e = db.db.Model(&conversationTurnModel{}).Where("session = ?", old).Update("session", next).Error; e != nil {
			t.Fatal(e)
		}
	}
	body, _ := json.Marshal(map[string]any{"input": visible})
	r := newJournalReplay(j)
	_, p, e := r.Prepare(context.Background(), model, "snapshot-shared", body, historydomain.ReplayPreparation{LegacyKeys: keys})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Discard()
	if p.RestoredItems() != 10 || p.Outcome() != "append_ok" {
		t.Fatalf("restored=%d outcome=%s", p.RestoredItems(), p.Outcome())
	}
	var last conversationTurnModel
	if e = db.db.First(&last, "id = ?", nodes[9].ID).Error; e != nil {
		t.Fatal(e)
	}
	recovered, e := j.loadChain(db.db, last.Session, last.Generation, last)
	if e != nil {
		t.Fatal(e)
	}
	if len(recovered) != 10 {
		t.Fatalf("recovered chain=%d", len(recovered))
	}
	for _, n := range nodes {
		var saved conversationTurnModel
		if e = db.db.First(&saved, "id = ?", n.ID).Error; e != nil {
			t.Fatal(e)
		}
		if saved.EncryptedOutput != n.EncryptedOutput || saved.ResponseID != n.ResponseID {
			t.Fatal("original output or response ID changed")
		}
	}
	t.Log("10 original turns reconnected; 10 reasoning items restored; original encrypted outputs and response IDs unchanged")
}
