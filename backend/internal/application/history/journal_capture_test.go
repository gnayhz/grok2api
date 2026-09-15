package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type captureJournal struct {
	repository.ConversationJournal
	commitErr error
	commits   int
}

func (j *captureJournal) Commit(context.Context, repository.JournalCommit) error {
	j.commits++
	return j.commitErr
}
func (*captureJournal) Release(context.Context, repository.JournalTicket) error { return nil }

func TestJournalCommitFailureDiagnostics(t *testing.T) {
	const sensitive = "synthetic-private-output"
	valid := `{"id":"synthetic-response","status":"completed","output":[{"role":"assistant","content":"` + sensitive + `"}]}`
	storageError := errors.New("synthetic-private-connection-details")
	for _, tc := range []struct {
		name, payload, stage, reason string
		streaming, unread, discard   bool
		budget                       int64
		storeErr                     error
	}{
		{name: "invalid_opaque", payload: `{"output":[{"type":"reasoning","encrypted_content":{"value":"` + sensitive + `"}}]}`, stage: "validate", reason: "invalid_opaque_reasoning"},
		{name: "missing_terminal", payload: "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"role\":\"assistant\",\"content\":\"" + sensitive + "\"}}\n\n", streaming: true, stage: "extract", reason: "missing_terminal_response"},
		{name: "unread", payload: valid, unread: true, stage: "capture", reason: "incomplete_capture"},
		{name: "discarded", payload: valid, discard: true, stage: "capture", reason: "discarded"},
		{name: "capture_budget", payload: valid, budget: 128, stage: "capture", reason: "resource_exhausted"},
		{name: "decode_budget", payload: valid, budget: 512, stage: "decode", reason: "resource_exhausted"},
		{name: "size_limit", payload: strings.Repeat("x", maxReplayCaptureBytes+1), stage: "capture", reason: "size_limit"},
		{name: "store", payload: valid, storeErr: storageError, stage: "store", reason: "store_error"},
		{name: "deadline", payload: valid, storeErr: context.DeadlineExceeded, stage: "store", reason: "deadline_exceeded"},
		{name: "stale", payload: valid, storeErr: historydomain.ErrHistoryStale, stage: "store", reason: "stale_generation"},
		{name: "quota", payload: valid, storeErr: historydomain.ErrHistoryQuota, stage: "store", reason: "quota_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			journal := &captureJournal{commitErr: tc.storeErr}
			replay := New(nil, Config{}, slog.New(slog.NewJSONHandler(&logs, nil)))
			replay.journal = journal
			prepared := &PreparedHistory{replay: replay, reservation: repository.JournalReservation{Ticket: repository.JournalTicket{
				Scope: repository.JournalScope{Model: "synthetic-model", Key: "synthetic-private-scope"}, Generation: 7,
			}}}
			var body io.ReadCloser = io.NopCloser(strings.NewReader(tc.payload))
			if tc.budget > 0 {
				body = responsebuffer.AttachBudget(body, responsebuffer.NewPool(tc.budget).Request(tc.budget))
			}
			capture, commit, discard := prepared.Capture(body, tc.streaming)
			defer discard()
			defer capture.Close()
			if !tc.unread {
				if _, err := io.Copy(io.Discard, capture); err != nil {
					t.Fatal(err)
				}
			}
			if tc.discard {
				discard()
			}
			err := commit()
			if !errors.Is(err, historydomain.ErrHistoryCommit) || commit() != err {
				t.Fatalf("commit failure was not stable: %v", err)
			}
			if tc.storeErr != nil && !errors.Is(err, tc.storeErr) {
				t.Fatalf("lost storage cause: %v", err)
			}
			if tc.stage != "store" && journal.commits != 0 || tc.stage == "store" && journal.commits != 1 {
				t.Fatalf("unexpected storage calls: %d", journal.commits)
			}
			text := logs.String()
			for _, secret := range []string{sensitive, storageError.Error(), "synthetic-private-scope"} {
				if strings.Contains(text, secret) || strings.Contains(err.Error(), secret) {
					t.Fatal("commit diagnostic exposed private content")
				}
			}
			failures := 0
			decoder := json.NewDecoder(strings.NewReader(text))
			for decoder.More() {
				var record map[string]any
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				if record["msg"] != "conversation_history_commit_failed" {
					continue
				}
				failures++
				if record["stage"] != tc.stage || record["reason"] != tc.reason || record["scope_hash"] != prepared.ScopeHash() || record["generation"] != float64(7) {
					t.Fatalf("incorrect failure diagnostic: %v", record)
				}
			}
			if failures != 1 {
				t.Fatalf("duplicate or missing failure log: %d", failures)
			}
		})
	}
}

func TestJournalCommitConcurrentCallersShareFailure(t *testing.T) {
	var logs bytes.Buffer
	journal := &captureJournal{commitErr: historydomain.ErrHistoryStale}
	replay := New(nil, Config{}, slog.New(slog.NewJSONHandler(&logs, nil)))
	replay.journal = journal
	prepared := &PreparedHistory{replay: replay}
	capture, commit, discard := prepared.Capture(io.NopCloser(strings.NewReader(`{"output":[{"role":"assistant","content":"synthetic answer"}]}`)), false)
	defer discard()
	defer capture.Close()
	if _, err := io.Copy(io.Discard, capture); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := commit(); !errors.Is(err, historydomain.ErrHistoryStale) {
				t.Errorf("lost stale generation: %v", err)
			}
		})
	}
	wg.Wait()
	if journal.commits != 1 || strings.Count(logs.String(), "conversation_history_commit_failed") != 1 {
		t.Fatal("repeated completion produced duplicate writes or diagnostics")
	}
}
