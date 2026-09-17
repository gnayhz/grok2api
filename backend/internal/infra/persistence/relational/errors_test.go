package relational

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

type mapTemporaryError struct{ message string }

func (e mapTemporaryError) Error() string { return e.message }
func (mapTemporaryError) Temporary() bool { return true }

type mapTimeoutError struct{ message string }

func (e mapTimeoutError) Error() string { return e.message }
func (mapTimeoutError) Timeout() bool   { return true }
func (mapTimeoutError) Temporary() bool { return false }

type mapSQLiteError struct{ code int }

func (e mapSQLiteError) Error() string { return "sqlite failure" }
func (e mapSQLiteError) Code() int     { return e.code }

type mapPostgresError struct{ state string }

func (e mapPostgresError) Error() string    { return "postgres failure" }
func (e mapPostgresError) SQLState() string { return e.state }

func TestMapErrorClassifiesDriverFaultsIntoStableKinds(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind repository.StoreFaultKind
	}{
		{name: "bad connection", err: driver.ErrBadConn, kind: repository.StoreFaultConnection},
		{name: "connection done", err: sql.ErrConnDone, kind: repository.StoreFaultConnection},
		{name: "sqlite busy", err: mapSQLiteError{code: 5}, kind: repository.StoreFaultLock},
		{name: "sqlite locked", err: mapSQLiteError{code: 6}, kind: repository.StoreFaultLock},
		{name: "sqlite extended locked", err: mapSQLiteError{code: 6 | 1<<8}, kind: repository.StoreFaultLock},
		{name: "postgres connection class", err: mapPostgresError{state: "08006"}, kind: repository.StoreFaultConnection},
		{name: "postgres admin shutdown", err: mapPostgresError{state: "57P01"}, kind: repository.StoreFaultConnection},
		{name: "postgres cannot connect now", err: mapPostgresError{state: "57P03"}, kind: repository.StoreFaultConnection},
		{name: "postgres serialization", err: mapPostgresError{state: "40001"}, kind: repository.StoreFaultSerialization},
		{name: "postgres deadlock", err: mapPostgresError{state: "40P01"}, kind: repository.StoreFaultSerialization},
		{name: "postgres lock not available", err: mapPostgresError{state: "55P03"}, kind: repository.StoreFaultLock},
		{name: "postgres check constraint", err: mapPostgresError{state: "23514"}, kind: repository.StoreFaultConstraint},
		{name: "network timeout", err: mapTimeoutError{message: "i/o timeout"}, kind: repository.StoreFaultTimeout},
		{name: "temporary marker", err: mapTemporaryError{message: "retry"}, kind: repository.StoreFaultConnection},
		{name: "wrapped fault keeps kind", err: fmt.Errorf("load: %w", mapSQLiteError{code: 5}), kind: repository.StoreFaultLock},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped := mapError(test.err)
			kind, ok := repository.StoreFaultKindOf(mapped)
			if !ok || kind != test.kind {
				t.Fatalf("mapError(%v) kind = %q, %v; want %q", test.err, kind, ok, test.kind)
			}
			if !errors.Is(mapped, test.err) && test.err != driver.ErrBadConn {
				// The cause must stay reachable through the fault wrapper.
				var fault *repository.StoreFault
				if errors.As(mapped, &fault) && fault.Cause == nil {
					t.Fatal("fault lost its cause")
				}
			}
		})
	}
}

func TestMapErrorLeavesNonStoreConditionsAlone(t *testing.T) {
	if got := mapError(context.Canceled); got != context.Canceled {
		t.Fatalf("caller cancellation must stay a context error, got %v", got)
	}
	if got := mapError(context.DeadlineExceeded); got != context.DeadlineExceeded {
		t.Fatalf("caller deadline must stay a context error, got %v", got)
	}
	// PostgreSQL query_canceled is the caller's own deadline, not a store fault.
	if _, ok := repository.StoreFaultKindOf(mapError(mapPostgresError{state: "57014"})); ok {
		t.Fatal("query_canceled must not become a store fault")
	}
	sentinel := errors.New("schema bug")
	if got := mapError(sentinel); got != sentinel {
		t.Fatalf("unknown errors pass through unchanged, got %v", got)
	}
	if _, ok := repository.StoreFaultKindOf(mapError(mapSQLiteError{code: 19})); ok {
		t.Fatal("sqlite constraint violation is not classified as transient")
	}
	if _, ok := repository.StoreFaultKindOf(mapError(mapPostgresError{state: "42601"})); ok {
		t.Fatal("syntax error is not classified as transient")
	}
	if _, ok := repository.StoreFaultKindOf(nil); ok {
		t.Fatal("nil error is not a store fault")
	}
}

// net.Error compliance check for the timeout fixture.
var _ net.Error = mapTimeoutError{}
