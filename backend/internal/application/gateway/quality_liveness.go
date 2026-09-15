package gateway

import (
	"sync"
	"time"
)

// One timer follows the admission phase: first data, then useful evidence.
// The canonical reader changes phase while its caller waits on the timer.
type qualityLivenessTimer struct {
	mu              sync.Mutex
	timer           *time.Timer
	deadline        time.Time
	evidenceTimeout time.Duration
	sawData         bool
}

func newQualityLivenessTimer(cfg QualityRetryRuntime) *qualityLivenessTimer {
	deadline := time.Now().Add(cfg.CreatedTimeout)
	return &qualityLivenessTimer{
		timer:           time.NewTimer(time.Until(deadline)),
		deadline:        deadline,
		evidenceTimeout: cfg.EvidenceTimeout,
	}
}

// Only the first data event starts the evidence phase. Metadata and keepalives
// cannot renew the budget, and a late event cannot revive an expired phase.
func (l *qualityLivenessTimer) observeData() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sawData {
		return nil
	}
	if err := l.timeoutLocked(); err != nil {
		return err
	}
	l.sawData = true
	l.deadline = time.Now().Add(l.evidenceTimeout)
	l.timer.Reset(time.Until(l.deadline))
	return nil
}

func (l *qualityLivenessTimer) timeout() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.timeoutLocked()
}

func (l *qualityLivenessTimer) timeoutLocked() error {
	// The caller may already have selected the old timer when the reader
	// starts the evidence phase. Recheck its current deadline under the lock.
	if time.Now().Before(l.deadline) {
		return nil
	}
	if l.sawData {
		return errQualityEvidenceTimeout
	}
	return errQualityCreatedTimeout
}
