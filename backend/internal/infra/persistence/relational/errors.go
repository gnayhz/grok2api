package relational

import (
	"errors"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/sqlfault"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return repository.ErrNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return repository.ErrConflict
	}
	if fault := classifyStoreFault(err); fault != nil {
		return fault
	}
	return err
}

// Keep the repository boundary delegating to the shared SQL classifier.
func classifyStoreFault(err error) *repository.StoreFault { return sqlfault.Classify(err) }
