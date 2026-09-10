package repository

import (
	"context"
	"time"
)

const QualitySettingsKey = "quality_tunables"

// QualitySettingsSchemaVersion identifies the durable quality configuration
// encoding. Version 2 validates explicit zeros; legacy zeros meant defaults.
const QualitySettingsSchemaVersion = 2

// SettingsDocument is a persisted configuration and its durable CAS clock.
// Payload interpretation belongs to its owning module, not the SQL mechanism.
type SettingsDocument struct {
	Payload   []byte
	Revision  uint64
	UpdatedAt time.Time
}

// SettingsDocumentRepository is bound to one configuration key at composition.
// A missing document has revision 0; every successful write advances it.
type SettingsDocumentRepository interface {
	Load(context.Context) (SettingsDocument, error)
	Save(context.Context, []byte, uint64) (SettingsDocument, error)
}
