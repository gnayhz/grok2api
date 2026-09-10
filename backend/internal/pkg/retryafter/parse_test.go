package retryafter

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHeaderPreservesDelayOrderingAtNumericBounds(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	maxSeconds := int64(math.MaxInt64) / int64(time.Second)
	for _, tc := range []struct {
		input string
		want  time.Duration
	}{
		{"", 0}, {"0", 0}, {"-1", 0}, {"-0", 0}, {"1.5", 0}, {"1x", 0}, {"NaN", 0}, {"+", 0},
		{"1", time.Second}, {"  +00300 ", 300 * time.Second},
		{fmt.Sprint(maxSeconds), time.Duration(maxSeconds) * time.Second},
		{fmt.Sprint(maxSeconds + 1), time.Duration(math.MaxInt64)},
		{"18446744193", time.Duration(math.MaxInt64)},
		{strings.Repeat("9", 128), time.Duration(math.MaxInt64)},
		{strings.Repeat("9", 128) + "x", 0},
		{now.Add(10 * time.Minute).Format(http.TimeFormat), 10 * time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"Wed, 31 Dec 9999 23:59:59 GMT", time.Duration(math.MaxInt64)},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := Header(tc.input, now); got != tc.want {
				t.Fatalf("Header(%q)=%v want=%v", tc.input, got, tc.want)
			}
		})
	}
}

func TestResetTextSaturatesEachUnitAndSum(t *testing.T) {
	maxValue := time.Duration(math.MaxInt64)
	for _, tc := range []struct {
		input string
		want  time.Duration
	}{
		{"ordinary text 30s", 0}, {"Resets in:", 0}, {"Resets in: 0s", 0},
		{"prefix RESETS IN: 1d 2H 3 m 4s", 26*time.Hour + 3*time.Minute + 4*time.Second},
		{"Resets in: 18446744193s", maxValue},
		{"Resets in: 18446744193m", maxValue},
		{"Resets in: 18446744193h", maxValue},
		{"Resets in: 18446744193d", maxValue},
		{"Resets in: " + strings.Repeat("9", 128) + "s", maxValue},
		{"Resets in: 9223372036s 1s", maxValue},
		{"Resets in: 153722867m 16s", 153722867*time.Minute + 16*time.Second},
		{"Resets in: 153722867m 16s 10s", maxValue},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := ResetText(tc.input); got != tc.want {
				t.Fatalf("ResetText(%q)=%v want=%v", tc.input, got, tc.want)
			}
		})
	}
}

func TestSecondsCeilAtBounds(t *testing.T) {
	for _, tc := range []struct {
		delay time.Duration
		want  int64
	}{
		{-1, 0}, {0, 0}, {1, 1}, {time.Second, 1}, {time.Second + 1, 2}, {maximum, 9223372037},
	} {
		if got := SecondsCeil(tc.delay); got != tc.want {
			t.Errorf("%v: got %d want %d", tc.delay, got, tc.want)
		}
	}
}
