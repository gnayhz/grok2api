// Package retryafter parses upstream delay syntax without selecting a retry
// policy. Positive values beyond Duration's range saturate instead of wrapping.
package retryafter

import (
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maximum = time.Duration(math.MaxInt64)

var resetPattern = regexp.MustCompile(`(?i)(\d+)\s*([dhms])`)

// Header accepts the existing integer-seconds and HTTP-date forms. Invalid,
// zero and expired values yield zero. A larger delay never becomes a smaller
// value through integer conversion; callers retain their own retry limits.
func Header(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if delay, ok := scaledDecimal(value, time.Second); ok {
		return delay
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// ResetText reads the existing "Resets in:" d/h/m/s wire syntax. Multiplication
// and addition saturate independently, including decimal values larger than an
// int64. Text recognition does not provide a default delay or minimum cooldown.
func ResetText(value string) time.Duration {
	index := strings.Index(strings.ToLower(value), "resets in:")
	if index < 0 {
		return 0
	}
	value = value[index+len("resets in:"):]
	var total time.Duration
	for _, match := range resetPattern.FindAllStringSubmatch(value, -1) {
		unit := time.Second
		switch strings.ToLower(match[2]) {
		case "d":
			unit = 24 * time.Hour
		case "h":
			unit = time.Hour
		case "m":
			unit = time.Minute
		}
		part, _ := scaledDecimal(match[1], unit)
		if part > maximum-total {
			return maximum
		}
		total += part
	}
	return total
}

func scaledDecimal(value string, unit time.Duration) (time.Duration, bool) {
	if strings.HasPrefix(value, "+") {
		value = value[1:]
	}
	if value == "" {
		return 0, false
	}
	limit := uint64(maximum / unit)
	var number uint64
	overflow := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if overflow {
			continue
		}
		digit := uint64(c - '0')
		if number > limit/10 || number == limit/10 && digit > limit%10 {
			overflow = true
			continue
		}
		number = number*10 + digit
	}
	if overflow {
		return maximum, true
	}
	return time.Duration(number) * unit, true
}

// SecondsCeil encodes a nonnegative delay in whole seconds without adding to the
// duration first. Positive fractions need one further second, even at maximum.
func SecondsCeil(delay time.Duration) int64 {
	if delay <= 0 {
		return 0
	}
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	return seconds
}
