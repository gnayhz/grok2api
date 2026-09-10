package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type faultyJournalHot struct {
	*memory.ReasoningReplayStore
	mode string
}

func (h *faultyJournalHot) Get(ctx context.Context, model, key string, now time.Time, ttl time.Duration) ([][]byte, bool, error) {
	switch h.mode {
	case "corrupt":
		return [][]byte{[]byte("corrupted secret")}, true, nil
	case "slow":
		<-ctx.Done()
		return nil, false, ctx.Err()
	default:
		return nil, false, errors.New("cache unavailable")
	}
}
func (h *faultyJournalHot) Set(context.Context, string, string, [][]byte, time.Time) error {
	return errors.New("cache write unavailable")
}
func TestConversationJournalHotFailureFallsBackToDurable(t *testing.T) {
	_, j, _ := journalFixture(t)
	ctx := context.Background()
	root, err := j.Reserve(ctx, journalParams("root", "question"))
	if err != nil {
		t.Fatal(err)
	}
	journalComplete(t, j, root, "durable answer")
	for _, mode := range []string{"corrupt", "unavailable", "slow"} {
		t.Run(mode, func(t *testing.T) {
			j.WithHotCache(&faultyJournalHot{ReasoningReplayStore: memory.NewReasoningReplayStore(10), mode: mode}, time.Hour)
			started := time.Now()
			r, err := j.Reserve(ctx, journalParams(mode, "question", "durable answer", "next"))
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Turns) != 1 || string(r.Turns[0].Output[0]) != "durable answer" {
				t.Fatal("hot fault changed canonical history")
			}
			if time.Since(started) > time.Second {
				t.Fatal("hot read exceeded bounded request budget")
			}
			if err = j.Release(ctx, r.Ticket); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestConversationJournalFailedCommitPreservesParent(t *testing.T) {
	db, j, _ := journalFixture(t)
	ctx := context.Background()
	root, err := j.Reserve(ctx, journalParams("root", "question"))
	if err != nil {
		t.Fatal(err)
	}
	journalComplete(t, j, root, "answer")
	next, err := j.Reserve(ctx, journalParams("rejected", "question", "answer", "next"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.db.Exec(`CREATE TRIGGER journal_disk_failure BEFORE INSERT ON conversation_journal_turns_v1 BEGIN SELECT RAISE(ABORT, 'simulated disk full'); END`).Error; err != nil {
		t.Fatal(err)
	}
	rejected := repository.JournalCommit{Ticket: next.Ticket, ResponseID: "rejected", PrefixHash: journalDigest(next.Ticket.InputHash + "rejected output"), TotalCount: next.Ticket.InputCount + 1, Output: [][]byte{[]byte("rejected output")}, Now: time.Now().UTC()}
	if err = j.Commit(ctx, rejected); err == nil {
		t.Fatal("database write failure accepted")
	}
	if err = db.db.Exec("DROP TRIGGER journal_disk_failure").Error; err != nil {
		t.Fatal(err)
	}
	retry, err := j.Reserve(ctx, journalParams("retry", "question", "answer", "retry"))
	if err != nil {
		t.Fatal(err)
	}
	if retry.Ticket.Parent != next.Ticket.Parent || len(retry.Turns) != 1 {
		t.Fatal("failed commit damaged accepted prefix")
	}
}
