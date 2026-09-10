package audit

import (
	"fmt"
	"time"
)

const DefaultRetentionPeriod = 7 * 24 * time.Hour

// RetentionPolicy is the audit owner's sole retention rule. Zero keeps records
// indefinitely. Billing ledger retention is independent of this policy.
type RetentionPolicy struct {
	Period time.Duration
}

func (p RetentionPolicy) Validate() error {
	if p.Period != 0 && (p.Period < 24*time.Hour || p.Period > 8760*time.Hour) {
		return fmt.Errorf("audit.retentionPeriod 必须为 0 或 24h 到 8760h 之间的时长")
	}
	return nil
}

func (p RetentionPolicy) Cutoff(now time.Time) (time.Time, bool) {
	if p.Period <= 0 {
		return time.Time{}, false
	}
	return now.UTC().Add(-p.Period), true
}
