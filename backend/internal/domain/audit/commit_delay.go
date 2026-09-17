package audit

import (
	"fmt"
	"time"
)

// Commit delay bounds the journal's durability window: sub-millisecond delays
// defeat batching, long delays stretch acknowledged writes. Zero means "keep
// the current value" on the editable surface and is resolved by the caller.
const (
	MinCommitDelay = time.Millisecond
	MaxCommitDelay = 50 * time.Millisecond
)

// ValidateCommitDelay is the single owner of the commit-delay policy; the
// settings merge and the file-baseline validator must both consult it.
func ValidateCommitDelay(delay time.Duration) error {
	if delay < MinCommitDelay || delay > MaxCommitDelay {
		return fmt.Errorf("audit.commitDelay 必须在 1ms 到 50ms 之间")
	}
	return nil
}
