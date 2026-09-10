package relational

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"math"
	"testing"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Reset keeps the clock, so both an existing service and a restarted service
// use the same next CAS revision. A missing row cannot revive a stale writer.
func TestRuntimeSettingsSaveAfterReset(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRuntimeSettingsRepository(database, cipher)

	// 首次保存(revision 0 → insert)与读取。
	if _, revision, err := repo.Save(ctx, settingsdomain.Config{}, 0); err != nil || revision != 1 {
		t.Fatalf("initial save: revision=%d err=%v", revision, err)
	}
	// Reset persists revision 2, visible to any new reader.
	if _, revision, err := repo.Reset(ctx, 1); err != nil || revision != 2 {
		t.Fatalf("reset revision=%d err=%v", revision, err)
	}
	if _, stamp, revision, found, err := repo.Get(ctx); err != nil || found || revision != 2 || stamp.IsZero() {
		t.Fatalf("reset read revision=%d found=%v err=%v", revision, found, err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, revision, found, err := repo.Get(ctx); err != nil || found || revision != 2 {
		t.Fatalf("migration changed reset revision=%d found=%v err=%v", revision, found, err)
	}
	// 此后首次保存带 expectedRevision=2:不得返回 ErrConflict。
	_, revision, saveErr := repo.Save(ctx, settingsdomain.Config{}, 2)
	if saveErr != nil {
		t.Fatalf("save after reset must update durable clock, got err=%v", saveErr)
	}
	if revision != 3 {
		t.Fatalf("revision continuity: got %d, want 3", revision)
	}
	// 行存在时的 revision 竞争语义保持:过期 expectedRevision 仍冲突。
	if _, _, err := repo.Save(ctx, settingsdomain.Config{}, 2); err == nil {
		t.Fatal("stale revision on existing row must conflict")
	}
}

func TestRuntimeSettingsMissingClockRejectsStaleWriter(t *testing.T) {
	ctx := context.Background()
	db := openTestDatabase(t)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRuntimeSettingsRepository(db, cipher)
	if _, _, err := repo.Save(ctx, settingsdomain.Config{}, 2); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("missing row save: %v", err)
	}
	if _, _, err := repo.Reset(ctx, 2); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("missing row reset: %v", err)
	}
	if _, _, err := repo.Reset(ctx, math.MaxInt64); err == nil {
		t.Fatal("revision overflow accepted")
	}
}
