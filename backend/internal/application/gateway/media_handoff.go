package gateway

import (
	"context"
	"io"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

// mediaHandoff owns a unary/streaming media body after account selection. The
// Provider still defines generation; the HTTP transport reports only delivery.
// It joins body producers before the modality-specific accounting snapshots.
type mediaHandoff struct {
	ctx                         context.Context
	response                    *provider.Response
	budget                      *inferencedomain.AttemptBudget
	release                     func()
	finish                      func(DeliveryStats, bool, string)
	once, releaseOnce           sync.Once
	mu                          sync.Mutex
	claimed, admitted, terminal bool
	delivery                    DeliveryStats
}

func (d *mediaHandoff) closeAndRelease() {
	d.releaseOnce.Do(func() {
		_ = d.response.Body.Close()
		d.release()
	})
}

func (d *mediaHandoff) finalize(_ Usage, _ string, code string) {
	d.once.Do(func() {
		d.mu.Lock()
		d.terminal = true
		stats, admitted := d.delivery, d.admitted
		d.mu.Unlock()
		defer d.budget.Close()
		d.closeAndRelease()
		d.finish(stats, admitted, code)
	})
}

func (d *mediaHandoff) begin() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.terminal || d.ctx.Err() != nil {
		return context.Canceled
	}
	d.claimed = true
	return nil
}

func (d *mediaHandoff) commit() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.terminal || d.ctx.Err() != nil {
		return context.Canceled
	}
	d.claimed = true
	d.admitted = d.response.StatusCode >= 200 && d.response.StatusCode < 300
	return nil
}

func (d *mediaHandoff) cancel() {
	d.mu.Lock()
	claimed, terminal := d.claimed, d.terminal
	if !claimed {
		d.terminal = true
	}
	d.mu.Unlock()
	if terminal {
		return
	}
	d.closeAndRelease()
	if !claimed {
		d.finalize(Usage{}, "", "request_canceled")
	}
}

func (d *mediaHandoff) result() *Result {
	d.response.Body = &onceCloseBody{ReadCloser: d.response.Body}
	stopCancel := context.AfterFunc(d.ctx, d.cancel)
	finalize := func(usage Usage, id, code string) { stopCancel(); d.finalize(usage, id, code) }
	return &Result{
		StatusCode: d.response.StatusCode, Status: d.response.Status, Header: d.response.Header,
		BeginDelivery: d.begin, CommitDelivery: d.commit,
		RecordDelivery: func(stats DeliveryStats) { d.mu.Lock(); d.delivery = stats; d.mu.Unlock() },
		Body:           &finalizingBody{ReadCloser: &mediaDeliveryBody{ReadCloser: d.response.Body, commit: d.commit}, finalize: func() { finalize(Usage{}, "", "stream_closed") }},
		Finalize:       finalize,
	}
}

// An embedded consumer claims the same delivery boundary on its first read;
// HTTP commits explicitly. Borrowing is used only after HTTP's commit.
type mediaDeliveryBody struct {
	io.ReadCloser
	commit func() error
}

func (b *mediaDeliveryBody) Read(p []byte) (int, error) {
	if err := b.commit(); err != nil {
		return 0, err
	}
	return b.ReadCloser.Read(p)
}
func (b *mediaDeliveryBody) BorrowBytes() ([]byte, func(), bool) {
	return responsebuffer.Borrow(b.ReadCloser)
}
func (b *mediaDeliveryBody) ResponseBudget() *responsebuffer.Budget {
	return responsebuffer.BudgetOf(b.ReadCloser)
}

func applyMediaDelivery(record *audit.Record, stats DeliveryStats, admitted bool, upstreamStatus int, code string) {
	record.StatusCode, record.UpstreamStatusCode = upstreamStatus, upstreamStatus
	if stats.StatusCode != 0 {
		record.StatusCode = stats.StatusCode
	}
	record.AdmissionOutcome = "not_admitted"
	if admitted {
		record.AdmissionOutcome = "admitted"
	}
	record.DeliveryOutcome = "failed"
	switch code {
	case "":
		if auditRequestSucceeded(upstreamStatus, code) {
			record.DeliveryOutcome = "completed"
		}
	case "client_disconnected", "request_canceled":
		record.DeliveryOutcome = "canceled"
		record.StatusCode = 499
	case "stream_closed":
		if stats.Bytes == 0 {
			record.DeliveryOutcome = "not_started"
		}
	}
	record.DeliveredBytes, record.DeliveredEvents = stats.Bytes, stats.Events
	record.ErrorCode = code
	record.HistoryCommit, record.ProviderStateCommit, record.OwnershipCommit, record.QualityReceipt = "not_required", "not_required", "not_required", "not_required"
}
