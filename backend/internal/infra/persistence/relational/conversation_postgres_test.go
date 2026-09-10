package relational

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestPostgresConversationJournalAcrossReplicas(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := OpenPostgres(ctx, dsn, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenPostgres(ctx, dsn, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = a.db.AutoMigrate(&conversationSessionModel{}, &conversationRequestModel{}, &conversationTurnModel{}); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewVersionedCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", nil)
	if err != nil {
		t.Fatal(err)
	}
	journals := []*ConversationJournal{NewConversationJournal(a, cipher, 64<<20), NewConversationJournal(b, cipher, 64<<20)}
	key := fmt.Sprintf("pg-history-%d", time.Now().UnixNano())
	params := func(token string, input ...string) repository.JournalReserve {
		p := journalParams(token, input...)
		p.Scope.Key = key
		return p
	}
	root, err := journals[0].Reserve(ctx, params("root", "question"))
	if err != nil {
		t.Fatal(err)
	}
	journalComplete(t, journals[0], root, "answer")
	const workers = 24
	var wg sync.WaitGroup
	failures := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j := journals[i%2]
			r, e := j.Reserve(ctx, params(fmt.Sprintf("child-%d", i), "question", "answer", fmt.Sprintf("next-%d", i)))
			if e != nil {
				failures <- e
				return
			}
			if r.Ticket.Parent != journalRequestID(journalScopeID(root.Ticket.Scope), "root") || len(r.Turns) != 1 {
				failures <- fmt.Errorf("replica lost parent")
				return
			}
			output := fmt.Sprintf("output-%d", i)
			failures <- j.Commit(ctx, repository.JournalCommit{Ticket: r.Ticket, ResponseID: fmt.Sprintf("response-%d", i), PrefixHash: journalDigest(r.Ticket.InputHash + output), TotalCount: r.Ticket.InputCount + 1, Output: [][]byte{[]byte(output)}, Now: time.Now().UTC()})
		}(i)
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal(e)
		}
	}
	var count int64
	if e := a.db.Model(&conversationTurnModel{}).Where("session = ?", journalScopeID(root.Ticket.Scope)).Count(&count).Error; e != nil {
		t.Fatal(e)
	}
	if count != workers+1 {
		t.Fatalf("lost concurrent commits: %d", count)
	}
	old, err := journals[0].Reserve(ctx, params("late", "question", "answer", "late"))
	if err != nil {
		t.Fatal(err)
	}
	if err = journals[1].Reset(ctx, root.Ticket.Scope, time.Now().UTC(), old.Ticket.Generation); err != nil {
		t.Fatal(err)
	}
	err = journals[0].Commit(ctx, repository.JournalCommit{Ticket: old.Ticket, ResponseID: "late", PrefixHash: "unused", TotalCount: old.Ticket.InputCount + 1, Output: [][]byte{[]byte("late")}, Now: time.Now().UTC()})
	if !errors.Is(err, historydomain.ErrHistoryStale) {
		t.Fatalf("cross-replica stale commit: %v", err)
	}
}
