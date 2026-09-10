//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package relational

import (
	"errors"
	"os"
)

func lockAuditJournal(*os.File) error {
	return errors.New("audit journal file locking is unavailable on this platform")
}
