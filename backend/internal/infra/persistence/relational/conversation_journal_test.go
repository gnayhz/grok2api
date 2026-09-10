package relational

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func journalFixture(t *testing.T) (*Database, *ConversationJournal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	db, e := OpenSQLite(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	if e = db.db.AutoMigrate(&conversationSessionModel{}, &conversationRequestModel{}, &conversationTurnModel{}); e != nil {
		t.Fatal(e)
	}
	cipher, e := security.NewVersionedCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", nil)
	if e != nil {
		t.Fatal(e)
	}
	return db, NewConversationJournal(db, cipher, 64<<20), path
}
func journalParams(token string, input ...string) repository.JournalReserve {
	p := repository.JournalReserve{Scope: repository.JournalScope{Key: "tenant-account-plane", Model: "model", Normalizer: 1}, Token: token, Now: time.Now().UTC(), Retention: 24 * time.Hour, Lease: time.Hour}
	hash := ""
	for _, v := range input {
		p.Input = append(p.Input, []byte(v))
		hash = journalDigest(hash + v)
		p.Prefixes = append(p.Prefixes, hash)
	}
	return p
}
func journalComplete(t *testing.T, j *ConversationJournal, r repository.JournalReservation, output string) repository.JournalCommit {
	t.Helper()
	p := repository.JournalCommit{Ticket: r.Ticket, ResponseID: r.Ticket.Token, PrefixHash: journalDigest(r.Ticket.InputHash + output), TotalCount: r.Ticket.InputCount + 1, Output: [][]byte{[]byte(output)}, Now: time.Now().UTC()}
	if e := j.Commit(context.Background(), p); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestConversationJournalLongHistoryRestartAndEncryption(t *testing.T) {
	db, j, path := journalFixture(t)
	ctx := context.Background()
	var input []string
	for turn := 0; turn < 200; turn++ {
		input = append(input, fmt.Sprintf("private-user-%d", turn))
		p := journalParams(fmt.Sprintf("t%d", turn), input...)
		r, e := j.Reserve(ctx, p)
		if e != nil {
			t.Fatal(e)
		}
		if len(r.Turns) != turn {
			t.Fatalf("turn %d: restored %d", turn, len(r.Turns))
		}
		output := fmt.Sprintf("private-assistant-%d-%s", turn, strings.Repeat("x", 1024))
		journalComplete(t, j, r, output)
		input = append(input, output)
	}
	var nodes []conversationTurnModel
	if e := db.db.Find(&nodes).Error; e != nil {
		t.Fatal(e)
	}
	total := 0
	for _, n := range nodes {
		total += len(n.EncryptedInput) + len(n.EncryptedOutput)
		if strings.Contains(n.EncryptedInput, "private-user") || strings.Contains(n.EncryptedOutput, "private-assistant") {
			t.Fatal("plaintext persisted")
		}
	}
	if total < 96<<10 || total > 1<<20 {
		t.Fatalf("delta storage not linear: %d bytes", total)
	}
	if e := db.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := OpenSQLite(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	next := NewConversationJournal(reopened, j.cipher, 64<<20)
	input = append(input, "after-restart")
	r, e := next.Reserve(ctx, journalParams("restart", input...))
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Turns) != 200 || string(r.Turns[0].Input[0]) != "private-user-0" {
		t.Fatalf("restart lost history: %d", len(r.Turns))
	}
}
func TestConversationJournalResetRejectsInflightAndIdempotentCommit(t *testing.T) {
	_, j, _ := journalFixture(t)
	ctx := context.Background()
	p := journalParams("first", "q")
	r, e := j.Reserve(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	c := journalComplete(t, j, r, "a")
	if e = j.Commit(ctx, c); e != nil {
		t.Fatalf("idempotent commit: %v", e)
	}
	c.Output = [][]byte{[]byte("changed")}
	if e = j.Commit(ctx, c); !errors.Is(e, repository.ErrConflict) {
		t.Fatalf("changed output accepted: %v", e)
	}
	pending, e := j.Reserve(ctx, journalParams("pending", "q", "a", "later"))
	if e != nil {
		t.Fatal(e)
	}
	if e = j.Reset(ctx, p.Scope, time.Now()); e != nil {
		t.Fatal(e)
	}
	if e = j.Commit(ctx, repository.JournalCommit{Ticket: pending.Ticket, ResponseID: "late", Output: [][]byte{[]byte("late")}, PrefixHash: "hash", TotalCount: 4, Now: time.Now()}); !errors.Is(e, historydomain.ErrHistoryStale) {
		t.Fatalf("old generation commit: %v", e)
	}
	fresh, e := j.Reserve(ctx, journalParams("new", "q", "a", "later"))
	if e != nil {
		t.Fatal(e)
	}
	if len(fresh.Turns) != 0 || fresh.Ticket.Generation <= pending.Ticket.Generation {
		t.Fatal("reset resurrected prior history")
	}
}
func TestConversationJournalConcurrentSiblingsAndAmbiguity(t *testing.T) {
	db, j, _ := journalFixture(t)
	ctx := context.Background()
	root, e := j.Reserve(ctx, journalParams("root", "q"))
	if e != nil {
		t.Fatal(e)
	}
	journalComplete(t, j, root, "a")
	left, e := j.Reserve(ctx, journalParams("left", "q", "a", "q2"))
	if e != nil {
		t.Fatal(e)
	}
	right, e := j.Reserve(ctx, journalParams("right", "q", "a", "q2"))
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, r := range []repository.JournalReservation{right, left} {
		wg.Add(1)
		go func(r repository.JournalReservation) {
			defer wg.Done()
			errs <- j.Commit(ctx, repository.JournalCommit{Ticket: r.Ticket, ResponseID: r.Ticket.Token, PrefixHash: journalDigest(r.Ticket.InputHash + "same"), TotalCount: 4, Output: [][]byte{[]byte(r.Ticket.Token)}, Now: time.Now()})
		}(r)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var s conversationSessionModel
	if e = db.db.First(&s).Error; e != nil {
		t.Fatal(e)
	}
	if s.Version != 2 {
		t.Fatalf("siblings both advanced head: %d", s.Version)
	}
	_, e = j.Reserve(ctx, journalParams("ambiguous", "q", "a", "q2", "same", "q3"))
	if !errors.Is(e, historydomain.ErrHistoryAmbiguous) {
		t.Fatalf("ambiguous siblings: %v", e)
	}
	p := journalParams("explicit", "q", "a", "q2", "same", "q3")
	p.ParentResponseID = "left"
	selected, e := j.Reserve(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if len(selected.Turns) != 2 || string(selected.Turns[1].Output[0]) != "left" {
		t.Fatal("explicit parent selected wrong sibling")
	}
}
func TestConversationJournalQuotaAndScopeIsolation(t *testing.T) {
	db, j, _ := journalFixture(t)
	ctx := context.Background()
	p := journalParams("root", "secret")
	r, e := j.Reserve(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	journalComplete(t, j, r, "answer")
	isolated := journalParams("other", "secret", "answer", "next")
	isolated.Scope.Key = "other-tenant-account"
	other, e := j.Reserve(ctx, isolated)
	if e != nil {
		t.Fatal(e)
	}
	if len(other.Turns) != 0 {
		t.Fatal("cross-scope history")
	}
	limited := NewConversationJournal(db, j.cipher, 1)
	if _, e = limited.Reserve(ctx, journalParams("quota", "secret", "answer", "next")); !errors.Is(e, historydomain.ErrHistoryQuota) {
		t.Fatalf("quota: %v", e)
	}
	restored, e := j.Reserve(ctx, journalParams("verify", "secret", "answer", "next"))
	if e != nil {
		t.Fatal(e)
	}
	if len(restored.Turns) != 1 {
		t.Fatal("quota deleted retained history")
	}
}
func TestConversationJournalPruneProtectsActiveLease(t *testing.T) {
	db, j, _ := journalFixture(t)
	ctx := context.Background()
	p := journalParams("pending", "q")
	p.Retention = time.Minute
	p.Lease = time.Hour
	r, e := j.Reserve(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if n, e := j.Prune(ctx, p.Now.Add(2*time.Minute), 100); e != nil || n != 0 {
		t.Fatalf("pruned active reservation: %d %v", n, e)
	}
	if n, e := j.Prune(ctx, p.Now.Add(2*time.Hour), 100); e != nil || n != 1 {
		t.Fatalf("expired prune: %d %v", n, e)
	}
	var count int64
	db.db.Model(&conversationRequestModel{}).Count(&count)
	if count != 0 {
		t.Fatal("orphan reservation")
	}
	if e = j.Commit(ctx, repository.JournalCommit{Ticket: r.Ticket, Now: p.Now.Add(2 * time.Hour)}); !errors.Is(e, historydomain.ErrHistoryMissing) {
		t.Fatalf("late commit resurrected deleted scope: %v", e)
	}
}
