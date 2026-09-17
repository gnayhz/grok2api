package testsupport

import (
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

// NewPhysicalJournalFactory exposes the execution-owned journal factory to
// integration tests that drive transports against the real accounting
// contract. Production composition happens in app; tests assemble here so
// infrastructure tests never import application packages directly.
func NewPhysicalJournalFactory() physical.JournalFactory {
	return executionapp.NewPhysicalJournalFactory()
}
