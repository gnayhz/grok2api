package memory

import (
	"context"
	"errors"
	"time"
)

func (r *RateLimiter) AllowRolling(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	if limit <= 0 || window < time.Millisecond {
		return false, 0, errors.New("invalid rolling rate limit")
	}
	r.rollingMu.Lock()
	defer r.rollingMu.Unlock()
	if r.rolling == nil {
		r.rolling = make(map[string][]time.Time)
	}
	now := time.Now()
	slots := r.rolling[key]
	for len(slots) > 0 && !slots[0].After(now.Add(-window)) {
		slots = slots[1:]
	}
	if len(slots) >= limit {
		r.rolling[key] = slots
		return false, slots[0].Add(window).Sub(now), nil
	}
	r.rolling[key] = append(slots, now)
	return true, 0, nil
}
