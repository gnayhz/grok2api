package media

import (
	"errors"
	"time"
)

var ErrInvalidExecutionLimits = errors.New("invalid media execution limits")
var ErrPhysicalBudgetExhausted = errors.New("media physical call budget exhausted")

// ExecutionLimits is fixed when a claimed job first starts. Reserved permits
// include uncertain submissions; Confirmed counts acknowledged physical facts.
// Neither counter proves exactly-once upstream execution.
type ExecutionLimits struct {
	Version       uint8
	Deadline      *time.Time
	PhysicalLimit uint32
	Reserved      uint32
	Confirmed     uint32
}

func (v ExecutionLimits) Validate() error {
	if v == (ExecutionLimits{}) {
		return nil
	}
	if v.Version != 1 || v.Deadline == nil || v.Deadline.IsZero() || v.PhysicalLimit == 0 || v.PhysicalLimit > 1<<20 || v.Reserved > v.PhysicalLimit || v.Confirmed > v.Reserved {
		return ErrInvalidExecutionLimits
	}
	return nil
}
