package relational

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return repository.ErrNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return repository.ErrConflict
	}
	if fault := classifyStoreFault(err); fault != nil {
		return fault
	}
	return err
}

// classifyStoreFault is the single SQL boundary that translates driver-level
// transient conditions into the stable repository fault contract. Caller
// cancellations and deadlines stay context errors: they are not store faults
// and consumers keep deciding their meaning per operation.
func classifyStoreFault(err error) *repository.StoreFault {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) {
		return &repository.StoreFault{Kind: repository.StoreFaultConnection, Cause: err}
	}
	// modernc SQLite exposes the primary/extended result through Code().
	var sqliteError interface{ Code() int }
	if errors.As(err, &sqliteError) {
		switch sqliteError.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY / SQLITE_LOCKED
			return &repository.StoreFault{Kind: repository.StoreFaultLock, Cause: err}
		}
	}
	// pgx exposes SQLSTATE without a concrete PostgreSQL driver dependency.
	var postgresError interface{ SQLState() string }
	if errors.As(err, &postgresError) {
		switch state := postgresError.SQLState(); {
		case strings.HasPrefix(state, "08"), state == "57P01", state == "57P02", state == "57P03":
			return &repository.StoreFault{Kind: repository.StoreFaultConnection, Cause: err}
		case strings.HasPrefix(state, "40"):
			return &repository.StoreFault{Kind: repository.StoreFaultSerialization, Cause: err}
		case state == "55P03":
			return &repository.StoreFault{Kind: repository.StoreFaultLock, Cause: err}
		case state == "23514", state == "23503":
			return &repository.StoreFault{Kind: repository.StoreFaultConstraint, Cause: err}
		}
	}
	// net.Error.Temporary 已弃用且语义不良;超时归 StoreFaultTimeout,
	// 其余网络错误一律按连接类瞬态处理(下方历史 Temporary() 实现的
	// 兼容分支保留)。
	var networkError net.Error
	if errors.As(err, &networkError) {
		kind := repository.StoreFaultConnection
		if networkError.Timeout() {
			kind = repository.StoreFaultTimeout
		}
		return &repository.StoreFault{Kind: kind, Cause: err}
	}
	var temporary interface{ Temporary() bool }
	if errors.As(err, &temporary) && temporary.Temporary() {
		return &repository.StoreFault{Kind: repository.StoreFaultConnection, Cause: err}
	}
	return nil
}
