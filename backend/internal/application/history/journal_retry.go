package history

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Retry only the captured local commit, under its existing deadline. Commit
// checks the same ticket/response/output identity before inserting, including
// after a connection error with an unknown transaction outcome. Every attempt
// rechecks current generation and reservation expiry; no upstream is replayed.
func (p *PreparedHistory) commitStoredOutput(ctx context.Context, commit repository.JournalCommit) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		commit.Now = p.replay.now().UTC()
		err := p.replay.journal.Commit(ctx, commit)
		kind, classified := repository.StoreFaultKindOf(err)
		if err == nil || attempt >= 2 || !classified || !repository.StoreFaultTransient(kind) {
			return err
		}
		delay := time.Duration(1+attempt*2) * 50 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
