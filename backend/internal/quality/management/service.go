package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

const SettingsKey = repository.QualitySettingsKey

var ErrInvalidInput = errors.New("invalid quality settings")

// Snapshot separates the durable intent from this instance's last successful
// application. Pending changes survive restarts and are retried by reconciliation.
type Snapshot struct {
	Config          Config
	Applied         Config
	Revision        uint64
	AppliedRevision uint64
	UpdatedAt       time.Time
	ApplyPending    bool
	ApplyError      string
}

type Service struct {
	mu         sync.Mutex
	repository repository.SettingsDocumentRepository
	apply      func(context.Context, Config) error
	notify     func(context.Context)
	state      Snapshot
}

func New(repo repository.SettingsDocumentRepository, apply func(context.Context, Config) error, notify func(context.Context)) *Service {
	defaults := DefaultConfig()
	return &Service{repository: repo, apply: apply, notify: notify,
		state: Snapshot{Config: defaults, Applied: defaults, ApplyPending: true}}
}

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Read refreshes from storage even if notifications were lost. Apply failures
// are represented in the snapshot; storage/decode failures are returned.
func (s *Service) Read(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(ctx); err != nil {
		return s.state, err
	}
	return s.state, nil
}

func (s *Service) ReloadPersisted(ctx context.Context) error {
	state, err := s.Read(ctx)
	if err != nil {
		return err
	}
	if state.ApplyPending {
		return fmt.Errorf("quality settings revision %d pending: %s", state.Revision, state.ApplyError)
	}
	return nil
}

func (s *Service) reload(ctx context.Context) error {
	doc, err := s.repository.Load(ctx)
	if err != nil {
		return err
	}
	if doc.Revision < s.state.Revision {
		return errors.New("quality settings revision regressed")
	}
	if doc.Revision > s.state.Revision {
		cfg, err := decodePersisted(doc.Payload)
		if err != nil {
			return fmt.Errorf("decode quality settings: %w", err)
		}
		s.state.Config, s.state.Revision, s.state.UpdatedAt = cfg, doc.Revision, doc.UpdatedAt
		s.state.ApplyPending = true
	}
	if s.state.ApplyPending {
		s.runApply(ctx)
	}
	return nil
}

func (s *Service) Update(ctx context.Context, expected uint64, input Config) (Snapshot, error) {
	normalized, _, err := normalize(input)
	if err != nil {
		return s.Snapshot(), fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	payload, err := json.Marshal(persistedConfig{Config: normalized, SchemaVersion: repository.QualitySettingsSchemaVersion, NetworkCapacityMigrated: true})
	if err != nil {
		return s.Snapshot(), err
	}
	s.mu.Lock()
	doc, err := s.repository.Save(ctx, payload, expected)
	if err != nil {
		state := s.state
		s.mu.Unlock()
		return state, err
	}
	s.state.Config, s.state.Revision, s.state.UpdatedAt = normalized, doc.Revision, doc.UpdatedAt
	s.state.ApplyPending = true
	s.runApply(ctx)
	state := s.state
	s.mu.Unlock()
	if s.notify != nil {
		s.notify(ctx)
	}
	return state, nil
}

func (s *Service) runApply(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.state.ApplyError = fmt.Sprintf("quality settings apply panic: %v", recovered)
		}
	}()
	if s.apply != nil {
		if err := s.apply(ctx, s.state.Config); err != nil {
			s.state.ApplyError = err.Error()
			return
		}
	}
	s.state.Applied = s.state.Config
	s.state.AppliedRevision = s.state.Revision
	s.state.ApplyPending, s.state.ApplyError = false, ""
}
