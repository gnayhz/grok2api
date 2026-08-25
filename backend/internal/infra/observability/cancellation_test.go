package observability

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsShutdownCancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	deadlineCtx, dlCancel := context.WithTimeout(context.Background(), 0)
	defer dlCancel()

	wrapped := fmt.Errorf("query failed: %w", context.Canceled)

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "nil error never suppresses", ctx: canceled, err: nil, want: false},
		{name: "live context never suppresses", ctx: context.Background(), err: context.Canceled, want: false},
		{name: "canceled context with bare Canceled", ctx: canceled, err: context.Canceled, want: true},
		{name: "canceled context with wrapped Canceled", ctx: canceled, err: wrapped, want: true},
		{name: "canceled context with DeadlineExceeded stays reportable", ctx: canceled, err: context.DeadlineExceeded, want: false},
		{name: "canceled context with unrelated error stays reportable", ctx: canceled, err: errors.New("disk full"), want: false},
		{name: "deadline ctx with Canceled from parent teardown", ctx: deadlineCtx, err: context.Canceled, want: true},
		{name: "nil context treated as live", ctx: nil, err: context.Canceled, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsShutdownCancellation(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("IsShutdownCancellation(%v, %v) = %v, want %v", tc.ctx, tc.err, got, tc.want)
			}
		})
	}
}
