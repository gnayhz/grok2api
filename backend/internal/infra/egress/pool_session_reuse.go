package egress

import (
	"container/list"
	"strconv"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// sessionReuseRoutes owns bounded, process-local routing hints. They do not
// reserve sockets or authorize execution. Qualification is checked by the
// caller before selection and again at lease publication.
type sessionReuseRoutes struct {
	mu       sync.Mutex
	pins     map[poolSessionKey]*list.Element
	recent   list.List
	assigned map[uint64]int
}

type poolSessionKey struct {
	poolID  uint64
	session string
}

type poolSessionPin struct {
	key      poolSessionKey
	nodeID   uint64
	lastUsed time.Time
}

// selectNode returns a currently eligible pin, or allocates and records a new
// pin atomically. Allocation first compares in-flight leases, then retained
// session assignments. A session hash breaks ties independently of account
// identity and node order. No I/O or transport construction occurs under mu.
func (s *sessionReuseRoutes) selectNode(poolID uint64, session string, nodes []domain.Node, now time.Time, inflight func(uint64) int64) domain.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pins == nil {
		s.pins = make(map[poolSessionKey]*list.Element)
		s.assigned = make(map[uint64]int)
	}
	for oldest := s.recent.Front(); oldest != nil; oldest = s.recent.Front() {
		if now.Sub(oldest.Value.(*poolSessionPin).lastUsed) < sessionPinIdleTTL {
			break
		}
		s.remove(oldest)
	}
	// Callers can arrive at the mutex out of timestamp order. Keep the LRU
	// timestamps ordered so expiry cannot be hidden behind a newer entry.
	if newest := s.recent.Back(); newest != nil && now.Before(newest.Value.(*poolSessionPin).lastUsed) {
		now = newest.Value.(*poolSessionPin).lastUsed
	}
	key := poolSessionKey{poolID: poolID, session: session}
	if entry := s.pins[key]; entry != nil {
		pin := entry.Value.(*poolSessionPin)
		for _, node := range nodes {
			if node.ID == pin.nodeID {
				pin.lastUsed = now
				s.recent.MoveToBack(entry)
				return node
			}
		}
		// Exclusion, quarantine and membership changes all invalidate the hint.
		s.remove(entry)
	}
	if len(s.pins) >= maxSessionPinnedNodes {
		s.remove(s.recent.Front())
	}
	seed := strconv.FormatUint(poolID, 10) + ":" + session
	selected := nodes[0]
	load, count, score := inflight(selected.ID), s.assigned[selected.ID], affinityNodeScore(seed, selected.ID)
	for _, node := range nodes[1:] {
		nextLoad, nextCount, nextScore := inflight(node.ID), s.assigned[node.ID], affinityNodeScore(seed, node.ID)
		if nextLoad < load || (nextLoad == load && (nextCount < count || (nextCount == count && nextScore > score))) {
			selected, load, count, score = node, nextLoad, nextCount, nextScore
		}
	}
	pin := &poolSessionPin{key: key, nodeID: selected.ID, lastUsed: now}
	s.pins[key] = s.recent.PushBack(pin)
	s.assigned[selected.ID]++
	return selected
}

func (s *sessionReuseRoutes) remove(entry *list.Element) {
	pin := entry.Value.(*poolSessionPin)
	delete(s.pins, pin.key)
	s.assigned[pin.nodeID]--
	if s.assigned[pin.nodeID] == 0 {
		delete(s.assigned, pin.nodeID)
	}
	s.recent.Remove(entry)
}
