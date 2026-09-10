package relational

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	reasoningreplay "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestConversationMigrationSnapshot(t *testing.T) {
	path := os.Getenv("HISTORY_MIGRATION_SNAPSHOT")
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
	dbpath := filepath.Join(t.TempDir(), "copy.db")
	os.WriteFile(dbpath, raw, 0600)
	db, e := OpenSQLite(context.Background(), dbpath)
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
	j := NewConversationJournal(db, cipher, 64<<20)
	var sessions []conversationSessionModel
	db.db.Find(&sessions)
	for _, s := range sessions {
		var before []conversationTurnModel
		db.db.Where("session = ?", s.ID).Order("created_at").Find(&before)
		e = db.db.Transaction(func(tx *gorm.DB) error {
			return j.migrateVisibleHistory(tx, &s, repository.JournalReserve{BaseHash: journalDigest("conversation-visible-v1"), ItemHash: reasoningreplay.CanonicalHistoryItemHash})
		})
		if e != nil {
			t.Fatal(e)
		}
		if len(before) == 12 {
			var last conversationTurnModel
			db.db.First(&last, "id = ?", before[11].ID)
			chain, e := j.loadChain(db.db, s.ID, s.Generation, last)
			if e != nil {
				t.Fatal(e)
			}
			if len(chain) != 12 {
				t.Fatalf("recovered chain=%d want12", len(chain))
			}
			t.Log("affected 12-turn session: all 12 original nodes reachable, broken root reconnected")
		}
	}
}
