package responsebuffer

// State reserves conservative capacity for semantic state that can outlive an
// event's JSON workspace. It does not allocate a second copy of that state.
// Cumulative growth and a reusable high-water allocation have separate limits.
type State struct {
	budget              *Budget
	limit               int
	retained, temporary int
	reserved            int
	leases              []*Lease
}

func NewState(budget *Budget, limit int) *State {
	if budget == nil {
		budget = NewRequest()
	}
	return &State{budget: budget, limit: limit}
}
func (s *State) Grow(retained, reusable int) error {
	if retained < 0 || retained > s.limit-s.retained {
		return ErrLimit
	}
	next := s.retained + retained
	peak := max(s.temporary, reusable)
	needed := next + peak
	if needed > s.reserved {
		size := ((needed - s.reserved + 4095) / 4096) * 4096
		lease, err := s.budget.Reserve(size)
		if err != nil {
			return err
		}
		s.leases = append(s.leases, lease)
		s.reserved += size
	}
	s.retained, s.temporary = next, peak
	return nil
}
func (s *State) Close() {
	for _, lease := range s.leases {
		lease.Release()
	}
	s.leases = nil
	s.reserved = 0
	s.retained = 0
	s.temporary = 0
}
