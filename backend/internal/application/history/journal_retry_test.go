package history

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestdiag"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestHistoryPrepareResourceFailureIsDiagnosable(t *testing.T) {
	r := New(memory.NewReasoningReplayStore(1), Config{Enabled: true}, nil)
	r.UseJournal(&captureJournal{}, time.Hour, time.Minute)
	ctx := requestdiag.WithCollector(responsebuffer.WithContext(context.Background(), responsebuffer.NewPool(8).Request(8)))
	_, _, err := r.Prepare(ctx, "synthetic-model", "synthetic-session", []byte(`{"input":[{"role":"user","content":"fictional request"}]}`))
	if !errors.Is(err, historydomain.ErrHistoryPrepare) || !errors.Is(err, responsebuffer.ErrExhausted) {
		t.Fatalf("unexpected error: %v", err)
	}
	d := requestdiag.Snapshot(ctx)
	if len(d.Failures) != 1 || d.Failures[0].Component != "history" || d.Failures[0].Stage != "history_prepare" || d.Failures[0].Reason != "resource_exhausted" {
		t.Fatalf("unexpected diagnostic: %+v", d)
	}
}

type retryJournal struct {
	repository.ConversationJournal
	calls    []repository.JournalCommit
	failures []error
	cancel   context.CancelFunc
}

func (j *retryJournal) Commit(_ context.Context, p repository.JournalCommit) error {
	j.calls = append(j.calls, p)
	if j.cancel != nil {
		j.cancel()
	}
	if len(j.calls) <= len(j.failures) {
		return j.failures[len(j.calls)-1]
	}
	return nil
}
func TestCapturedHistoryRetryKeepsIdentityAndRespectsFailures(t *testing.T) {
	transient := &repository.StoreFault{Kind: repository.StoreFaultConnection, Cause: errors.New("synthetic connection failure")}
	for _, tc := range []struct {
		name     string
		failures []error
		cancel   bool
		calls    int
		want     error
	}{
		{"recover", []error{transient}, false, 2, nil},
		{"bounded", []error{transient, transient, transient}, false, 3, transient},
		{"stale", []error{historydomain.ErrHistoryStale}, false, 1, historydomain.ErrHistoryStale},
		{"conflict", []error{repository.ErrConflict}, false, 1, repository.ErrConflict},
		{"quota", []error{historydomain.ErrHistoryQuota}, false, 1, historydomain.ErrHistoryQuota},
		{"cancel", []error{transient}, true, 1, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			j := &retryJournal{failures: tc.failures}
			if tc.cancel {
				j.cancel = cancel
			}
			r := New(nil, Config{}, nil)
			r.journal = j
			p := &PreparedHistory{replay: r}
			value := repository.JournalCommit{Ticket: repository.JournalTicket{Token: "synthetic-ticket", Generation: 3, Version: 4}, ResponseID: "synthetic-response", PrefixHash: "synthetic-prefix", Output: [][]byte{[]byte("fictional output")}}
			err := p.commitStoredOutput(ctx, value)
			if !errors.Is(err, tc.want) || len(j.calls) != tc.calls {
				t.Fatalf("calls=%d error=%v", len(j.calls), err)
			}
			for _, got := range j.calls {
				if got.Now.IsZero() {
					t.Fatal("expiry not checked")
				}
				got.Now = time.Time{}
				if !reflect.DeepEqual(got, value) {
					t.Fatal("commit identity changed")
				}
			}
		})
	}
}
