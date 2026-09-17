package selector

import (
	"time"
)

const maxLocalQualityOwners = 8192

// The embedded gateway and a failed persistence attempt retain a short local
// hold without changing manual Enabled or account health. Production admission
// still requires its authoritative database check; this is only a local fence.
func (s *Selector) holdLocalQuality(accountID uint64, owner string, until time.Time) {
	s.qualityHoldsMu.Lock()
	defer s.qualityHoldsMu.Unlock()
	now := time.Now()
	ownerCount := 0
	for account, owners := range s.qualityHolds {
		for key, expiry := range owners {
			if !expiry.After(now) {
				delete(owners, key)
			}
		}
		if len(owners) == 0 {
			delete(s.qualityHolds, account)
		}
		ownerCount += len(owners)
	}
	if s.qualityHolds == nil {
		s.qualityHolds = make(map[uint64]map[string]time.Time)
	}
	if _, exists := s.qualityHolds[accountID][owner]; exists {
		return
	}
	if ownerCount >= maxLocalQualityOwners {
		// Retaining another owner would exceed local protection capacity. A
		// short global fence preserves rejection without evicting active owners.
		if until.After(s.qualityHoldOverflow) {
			s.qualityHoldOverflow = until
		}
		s.qualityHoldOverflows++
		return
	}
	if s.qualityHolds[accountID] == nil {
		s.qualityHolds[accountID] = make(map[string]time.Time)
	}
	if _, exists := s.qualityHolds[accountID][owner]; !exists {
		s.qualityHolds[accountID][owner] = until
	}
}

func (s *Selector) localQualityAllowed(accountID uint64, now time.Time) bool {
	s.qualityHoldsMu.Lock()
	defer s.qualityHoldsMu.Unlock()
	if s.qualityHoldOverflow.After(now) {
		return false
	}
	for _, until := range s.qualityHolds[accountID] {
		if until.After(now) {
			return false
		}
	}
	delete(s.qualityHolds, accountID)
	return true
}
