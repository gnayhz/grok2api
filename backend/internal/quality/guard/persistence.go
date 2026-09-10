package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"time"
)

const SettingsKey = "quality_guard"

// Payload ownership stays with guard. Keep the established JSON names and
// duration encoding so existing documents need no rewriting on upgrade.
type persistedConfig struct {
	EvidenceTimeout      time.Duration `json:"evidence_timeout"`
	CreatedTimeout       time.Duration `json:"created_timeout"`
	AdmissionTimeout     time.Duration `json:"admission_timeout"`
	ToolAdmissionTimeout time.Duration `json:"tool_admission_timeout"`
	AccountCooldown      time.Duration `json:"account_cooldown"`
	IdleAccountCooldown  time.Duration `json:"idle_account_cooldown"`

	Enabled         bool     `json:"enabled"`
	GuardedModels   []string `json:"guarded_models"`
	MaxAttempts     int      `json:"max_attempts"`
	ReasoningExpect bool     `json:"reasoning_expected"`
}

type DocumentStore struct {
	documents repository.SettingsDocumentRepository
}

func NewDocumentStore(documents repository.SettingsDocumentRepository) *DocumentStore {
	return &DocumentStore{documents: documents}
}

func (s *DocumentStore) LoadGuard(ctx context.Context) (Config, bool, error) {
	doc, err := s.documents.Load(ctx)
	if err != nil {
		return Config{}, false, err
	}
	if doc.Revision == 0 && len(doc.Payload) == 0 {
		return Config{}, false, nil
	}
	if doc.Revision == 0 {
		return Config{}, false, errors.New("guard: persisted document has no revision")
	}
	var payload persistedConfig
	if err := json.Unmarshal(doc.Payload, &payload); err != nil {
		return Config{}, false, fmt.Errorf("decode guard settings: %w", err)
	}
	return Config{Revision: doc.Revision, EvidenceTimeout: payload.EvidenceTimeout, CreatedTimeout: payload.CreatedTimeout,
		AdmissionTimeout: payload.AdmissionTimeout, ToolAdmissionTimeout: payload.ToolAdmissionTimeout,
		AccountCooldown: payload.AccountCooldown, IdleAccountCooldown: payload.IdleAccountCooldown,
		Enabled: payload.Enabled, GuardedModels: payload.GuardedModels, MaxAttempts: payload.MaxAttempts, ReasoningExpected: payload.ReasoningExpect}, true, nil
}

func (s *DocumentStore) SaveGuard(ctx context.Context, cfg Config) error {
	if cfg.Revision == 0 {
		return errors.New("guard: next revision must be explicit")
	}
	payload, err := json.Marshal(persistedConfig{
		EvidenceTimeout: cfg.EvidenceTimeout, CreatedTimeout: cfg.CreatedTimeout,
		AdmissionTimeout: cfg.AdmissionTimeout, ToolAdmissionTimeout: cfg.ToolAdmissionTimeout,
		AccountCooldown: cfg.AccountCooldown, IdleAccountCooldown: cfg.IdleAccountCooldown,
		Enabled: cfg.Enabled, GuardedModels: cfg.GuardedModels, MaxAttempts: cfg.MaxAttempts, ReasoningExpect: cfg.ReasoningExpected})
	if err != nil {
		return err
	}
	doc, err := s.documents.Save(ctx, payload, cfg.Revision-1)
	if errors.Is(err, repository.ErrConflict) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if doc.Revision != cfg.Revision {
		return errors.New("guard: unexpected persisted revision")
	}
	return nil
}
