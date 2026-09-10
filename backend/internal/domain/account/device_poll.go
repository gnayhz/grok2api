package account

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrDeviceSessionExpired = errors.New("device authorization session expired")
	ErrDevicePollTooSoon    = errors.New("device authorization polling is not due")
)

type DevicePollReceipt struct {
	SessionID string
	Token     string
}

type DevicePollCompletionKind string

const (
	DevicePollPending    DevicePollCompletionKind = "pending"
	DevicePollSlowDown   DevicePollCompletionKind = "slow_down"
	DevicePollFailed     DevicePollCompletionKind = "failed"
	DevicePollDenied     DevicePollCompletionKind = "denied"
	DevicePollAuthorized DevicePollCompletionKind = "authorized"
)

type DevicePollCompletion struct {
	Kind        DevicePollCompletionKind
	CompletedAt time.Time
}

// ClaimDevicePoll owns due-time and in-flight policy. Runtime adapters apply
// this transition atomically; they do not independently decide polling rules.
func ClaimDevicePoll(current DeviceSession, token string, now, leaseUntil time.Time) (DeviceSession, error) {
	if now.IsZero() || !leaseUntil.After(now) || strings.TrimSpace(token) == "" || current.Interval <= 0 {
		return DeviceSession{}, errors.New("invalid device poll claim")
	}
	if !now.Before(current.ExpiresAt) {
		return DeviceSession{}, ErrDeviceSessionExpired
	}
	if now.Before(current.NextPollAt) || current.PollToken != "" && now.Before(current.PollLeaseUntil) {
		return DeviceSession{}, ErrDevicePollTooSoon
	}
	current.PollToken, current.PollLeaseUntil = token, leaseUntil.UTC()
	current.NextPollAt = now.Add(current.Interval).UTC()
	return current, nil
}

// DeviceSessionRetentionUntil keeps an already claimed operation available
// for bounded completion without extending the authorization's logical expiry.
func DeviceSessionRetentionUntil(current DeviceSession) time.Time {
	if current.PollToken != "" && current.PollLeaseUntil.After(current.ExpiresAt) {
		return current.PollLeaseUntil
	}
	return current.ExpiresAt
}

// CompleteDevicePoll fences old or repeated receipts, including a new claim
// following lease expiry. Terminal results consume the session, never reopen it.
func CompleteDevicePoll(current DeviceSession, receipt DevicePollReceipt, event DevicePollCompletion) (next DeviceSession, applied, remove bool, err error) {
	if receipt.SessionID != current.ID || receipt.Token == "" || receipt.Token != current.PollToken {
		return current, false, false, nil
	}
	if event.CompletedAt.IsZero() {
		return current, false, false, errors.New("device poll completion requires a time")
	}
	if !event.CompletedAt.Before(DeviceSessionRetentionUntil(current)) {
		return current, false, false, nil
	}
	next = current
	switch event.Kind {
	case DevicePollPending, DevicePollFailed:
	case DevicePollSlowDown:
		next.Interval += 5 * time.Second
	case DevicePollDenied, DevicePollAuthorized:
		return next, true, true, nil
	default:
		return current, false, false, errors.New("invalid device poll completion")
	}
	if !event.CompletedAt.Before(current.ExpiresAt) {
		return next, true, true, nil
	}
	next.PollToken, next.PollLeaseUntil = "", time.Time{}
	if due := event.CompletedAt.Add(next.Interval); due.After(next.NextPollAt) {
		next.NextPollAt = due.UTC()
	}
	return next, true, false, nil
}
