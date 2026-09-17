package selector

import "time"

type LocalProtectionStats struct {
	Owners        int        `json:"owners"`
	Limit         int        `json:"limit"`
	Overflows     uint64     `json:"overflows"`
	OverflowUntil *time.Time `json:"overflowUntil,omitempty"`
}

func (s *Selector) localProtectionStats() LocalProtectionStats {
	s.qualityHoldsMu.Lock()
	defer s.qualityHoldsMu.Unlock()
	stats := LocalProtectionStats{Limit: maxLocalQualityOwners, Overflows: s.qualityHoldOverflows}
	now := time.Now()
	for _, owners := range s.qualityHolds {
		for _, until := range owners {
			if until.After(now) {
				stats.Owners++
			}
		}
	}
	if s.qualityHoldOverflow.After(now) {
		until := s.qualityHoldOverflow
		stats.OverflowUntil = &until
	}
	return stats
}
