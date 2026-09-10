package audit

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
)

func TestWriterStopsAfterCloseDuringPersistentDatabaseFailure(t *testing.T) {
	repo := &toggleAuditRepository{err: errors.New("database remains unavailable")}
	service := newTestService(t, repo, slog.Default(), 8, 1, time.Millisecond)
	startAuditService(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := service.Create(ctx, auditdomain.Record{EventID: "evt_shutdown_persistent_failure", RequestID: "shutdown", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC()})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed database unexpectedly acknowledged: %v", err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer closeCancel()
	closeErr := service.Close(closeCtx)
	if closeErr != nil {
		t.Fatalf("persistent database failure prevented close: %v", closeErr)
	}
	if service.pending.Snapshot().Records != 1 {
		t.Fatal("shutdown discarded accepted fact")
	}
	select {
	case <-service.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Close returned %v while the writer still owns an active SQL retry", closeErr)
	}
}

// A storage adapter that finishes its append after cancellation models an I/O
// call that cannot stop immediately. Close must report incomplete shutdown and
// leave dependencies owned until the append has actually returned.
type delayedPendingAppend struct {
	repository.AuditPendingStore
	appended chan struct{}
	release  chan struct{}
}

func (p *delayedPendingAppend) Append(ctx context.Context, value auditdomain.Record) (repository.AuditPendingEntry, error) {
	entry, err := p.AuditPendingStore.Append(ctx, value)
	close(p.appended)
	<-p.release
	return entry, err
}

func TestCloseWaitsForInFlightDurableAppend(t *testing.T) {
	service := newTestService(t, &toggleAuditRepository{}, slog.Default(), 8, 4, time.Second)
	pending := &delayedPendingAppend{AuditPendingStore: service.pending, appended: make(chan struct{}), release: make(chan struct{})}
	service.pending = pending
	startAuditService(t, service)
	result := make(chan error, 1)
	go func() {
		result <- service.Create(context.Background(), auditdomain.Record{EventID: "evt_append_shutdown_race"})
	}()
	select {
	case <-pending.appended:
	case <-time.After(time.Second):
		t.Fatal("append did not complete")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := service.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active appender was reported stopped: %v", err)
	}
	select {
	case <-service.done:
		t.Fatal("dependencies released while appender active")
	default:
	}
	close(pending.release)
	if err := <-result; !errors.Is(err, ErrWriterUnavailable) {
		t.Fatalf("create after shutdown: %v", err)
	}
	closeAuditService(t, service)
	if service.pending.Snapshot().Records != 1 {
		t.Fatal("accepted fact lost while shutdown interrupted append acknowledgement")
	}
}

type faultPendingStore struct {
	repository.AuditPendingStore
	failRead   atomic.Bool
	failReject atomic.Bool
}

func (p *faultPendingStore) ReadPending(ctx context.Context, after uint64, limit int) ([]repository.AuditPendingEntry, error) {
	if p.failRead.Swap(false) {
		return nil, errors.New("transient pending read failure")
	}
	return p.AuditPendingStore.ReadPending(ctx, after, limit)
}
func (p *faultPendingStore) Reject(ctx context.Context, id uint64, reason string) error {
	if p.failReject.Swap(false) {
		return errors.New("transient pending rejection write failure")
	}
	return p.AuditPendingStore.Reject(ctx, id, reason)
}

type invalidOnceAuditRepository struct {
	repository.AuditRepository
	calls atomic.Int32
}

func (r *invalidOnceAuditRepository) CreateBatch(_ context.Context, values []auditdomain.Record) error {
	r.calls.Add(1)
	for i, value := range values {
		if value.EventID == "evt_invalid_pending_record" {
			return &repository.InvalidBatchRecordError{Index: i, Err: repository.ErrInvalidRecord}
		}
	}
	return nil
}

func TestPendingReadAndRejectionFailuresRetainFactsAndRecover(t *testing.T) {
	repo := &invalidOnceAuditRepository{}
	service := newTestService(t, repo, slog.Default(), 8, 4, time.Millisecond)
	pending := &faultPendingStore{AuditPendingStore: service.pending}
	pending.failRead.Store(true)
	pending.failReject.Store(true)
	service.pending = pending
	service.UpdateWriterConfig(4, time.Millisecond, 50*time.Millisecond)
	startAuditService(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invalidResult := make(chan error, 1)
	go func() {
		invalidResult <- service.Create(ctx, auditdomain.Record{EventID: "evt_invalid_pending_record"})
	}()
	if err := service.Create(ctx, auditdomain.Record{EventID: "evt_valid_pending_record"}); err != nil {
		t.Fatal(err)
	}
	if err := <-invalidResult; !errors.Is(err, repository.ErrInvalidRecord) {
		t.Fatalf("invalid result: %v", err)
	}
	if state := pending.Snapshot(); state.Records != 1 || state.Rejected != 1 {
		t.Fatalf("retained state=%+v", state)
	}
	if repo.calls.Load() < 3 {
		t.Fatal("rejected storage failure did not replay the unchanged batch")
	}
}

func TestWriterRequiresDurableStoreAndPropagatesStartupFailure(t *testing.T) {
	service := NewService(&toggleAuditRepository{}, nil, nil, 4, time.Millisecond)
	if err := service.Start(context.Background()); err == nil {
		t.Fatal("writer started without durable storage")
	}
	closeAuditService(t, service)
	service = newTestService(t, &toggleAuditRepository{}, nil, 8, 4, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("startup discarded cancellation: %v", err)
	}
	if err := service.Create(context.Background(), auditdomain.Record{EventID: "evt_start_failed"}); !errors.Is(err, ErrWriterUnavailable) {
		t.Fatalf("failed startup accepted fact: %v", err)
	}
}

func TestAcceptedDuplicateTimeoutDoesNotConsumeNewCapacity(t *testing.T) {
	repo := newGatedAuditRepository()
	service := newTestService(t, repo, nil, 1, 1, time.Second)
	startAuditService(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	value := auditdomain.Record{EventID: "evt_accepted_duplicate"}
	first := make(chan error, 1)
	go func() { first <- service.Create(ctx, value) }()
	select {
	case <-repo.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	retryCtx, retryCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	err := service.Create(retryCtx, value)
	retryCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("duplicate before commit=%v", err)
	}
	if state := service.LedgerSnapshot(); state.Dropped != 0 || state.Irrecoverable || state.QueueDepth != 1 {
		t.Fatalf("duplicate misclassified as unaccepted new fact: %+v", state)
	}
	close(repo.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if state := service.LedgerSnapshot(); !state.Ready || state.QueueDepth != 0 {
		t.Fatalf("settled duplicate did not recover: %+v", state)
	}
}

func TestFullPendingQueueNeverEvictsAcceptedFact(t *testing.T) {
	repo := newGatedAuditRepository()
	service := newTestService(t, repo, nil, 1, 1, time.Second)
	service.UpdateLedgerConfig(LedgerConfig{Mode: LedgerModeObserve, UnhealthyGrace: time.Hour})
	startAuditService(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- service.Create(ctx, auditdomain.Record{EventID: "evt_full_queue_accepted"}) }()
	select {
	case <-repo.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	nextCtx, nextCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	err := service.Create(nextCtx, auditdomain.Record{EventID: "evt_full_queue_unaccepted"})
	nextCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("full queue ignored caller deadline: %v", err)
	}
	entries, err := service.pending.ReadPending(ctx, 0, 10)
	if err != nil || len(entries) != 1 || entries[0].Record.EventID != "evt_full_queue_accepted" {
		t.Fatalf("full queue replaced accepted fact: %+v %v", entries, err)
	}
	if err := service.CheckLedgerReady(); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("unaccepted fact did not block observe mode: %v", err)
	}
	close(repo.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if state := service.LedgerSnapshot(); state.QueueDepth != 0 || !state.Irrecoverable || state.Dropped != 1 {
		t.Fatalf("later SQL success hid an unaccepted fact: %+v", state)
	}
}

func TestStartupRepairRemainsBlockedUntilRetainedFactsAreResolved(t *testing.T) {
	repo := newGatedAuditRepository()
	service := newTestService(t, repo, nil, 8, 4, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entry, err := service.pending.Append(ctx, auditdomain.Record{EventID: "evt_retained_startup_repair"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.pending.Reject(ctx, entry.ID, "old format rejected"); err != nil {
		t.Fatal(err)
	}
	service.UpdateLedgerConfig(LedgerConfig{Mode: LedgerModeObserve, UnhealthyGrace: time.Hour})
	startAuditService(t, service)
	select {
	case <-repo.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := service.CheckLedgerReady(); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("startup retry cleared repair barrier before SQL resolved: %v", err)
	}
	close(repo.release)
	deadline := time.Now().Add(time.Second)
	for service.CheckLedgerReady() != nil {
		if time.Now().After(deadline) {
			t.Fatalf("successful repair did not restore readiness: %+v", service.LedgerSnapshot())
		}
		time.Sleep(time.Millisecond)
	}
}
