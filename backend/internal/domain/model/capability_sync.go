package model

import (
	"errors"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

var (
	ErrCapabilitySyncSuperseded = errors.New("model capability sync superseded or already completed")
	ErrCapabilitySyncExhausted  = errors.New("model capability sync revision exhausted")
)

// CapabilitySyncRef orders observations across processes without relying on
// their wall clocks. Only the latest unfinished observation may publish.
type CapabilitySyncRef struct {
	AccountID uint64
	Revision  uint64
}

// CapabilitySyncResult identifies the actual material used after credential
// preparation (which may refresh it). An error preserves the last successful
// capability snapshot; an empty successful set explicitly replaces it.
type CapabilitySyncResult struct {
	Credential account.CredentialRef
	Models     []string
	Err        error
}
