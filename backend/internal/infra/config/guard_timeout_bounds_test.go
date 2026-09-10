package config

import (
	"strings"
	"testing"
	"time"
)

// File bounds follow the same policy as management, including subsecond waits.
func TestValidateRequestRetryEvidenceTimeout(t *testing.T) {
	t.Parallel()
	base := func(d time.Duration) RequestRetryConfig {
		return RequestRetryConfig{Enabled: true, EvidenceTimeout: Duration(d)}
	}
	for _, invalid := range []time.Duration{-time.Nanosecond, 24*time.Hour + time.Nanosecond} {
		if err := validateRequestRetry(base(invalid)); err == nil || !strings.Contains(err.Error(), "evidenceTimeout") {
			t.Fatalf("evidence timeout %v should be rejected, got %v", invalid, err)
		}
	}
	for _, valid := range []time.Duration{0, 700 * time.Millisecond, time.Second, 3500 * time.Millisecond, 10 * time.Minute, 24 * time.Hour} {
		if err := validateRequestRetry(base(valid)); err != nil {
			t.Fatalf("evidence timeout %v should be accepted, got %v", valid, err)
		}
	}
}

func TestValidateRequestRetryCreatedTimeout(t *testing.T) {
	t.Parallel()
	base := func(d time.Duration) RequestRetryConfig {
		return RequestRetryConfig{Enabled: true, CreatedTimeout: Duration(d)}
	}
	for _, invalid := range []time.Duration{-time.Nanosecond, 24*time.Hour + time.Nanosecond} {
		if err := validateRequestRetry(base(invalid)); err == nil || !strings.Contains(err.Error(), "createdTimeout") {
			t.Fatalf("created timeout %v should be rejected, got %v", invalid, err)
		}
	}
	for _, valid := range []time.Duration{0, 700 * time.Millisecond, time.Second, 5 * time.Second, 10 * time.Minute, 24 * time.Hour} {
		if err := validateRequestRetry(base(valid)); err != nil {
			t.Fatalf("created timeout %v should be accepted, got %v", valid, err)
		}
	}
}
