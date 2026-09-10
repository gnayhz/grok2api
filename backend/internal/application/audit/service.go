package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrWriterUnavailable = errors.New("audit writer is not running")
	ErrInvalidCursor     = errors.New("审计游标无效")
	ErrInvalidFilter     = errors.New("审计筛选条件无效")
	ErrInvalidPeriod     = errors.New("审计时间范围无效")
	ErrLedgerUnavailable = errors.New("billing ledger is not ready")
)

type LedgerMode string

const (
	LedgerModeObserve LedgerMode = "observe"
	LedgerModeEnforce LedgerMode = "enforce"
)

type LedgerConfig struct {
	Mode                      LedgerMode
	FailureThreshold          int
	UnhealthyGrace            time.Duration
	QueueHighWatermarkPercent int
}

type LedgerSnapshot struct {
	Mode                LedgerMode
	Ready               bool
	Irrecoverable       bool
	QueueDepth          int
	QueueCapacity       int
	ConsecutiveFailures int
	RepairPending       bool
	Rejected            int
	PendingBytes        int64
	CapacityBytes       int64
	Dropped             uint64
	LastSuccessAt       time.Time
	LastFailureAt       time.Time
	UnhealthySince      time.Time
}

type Period string

const (
	Period24Hours Period = "24h"
	Period7Days   Period = "7d"
	Period30Days  Period = "30d"
	Period90Days  Period = "90d"
)

const (
	auditWriteTimeout        = 2 * time.Second
	auditWriteRetryBase      = 250 * time.Millisecond
	auditWriteRetryMax       = 5 * time.Second
	auditDefaultCommitDelay  = 5 * time.Millisecond
	auditSummaryTTL          = 10 * time.Second
	requestMethodLimit       = 16
	requestPathLimit         = 2048
	requestHeaderNameLimit   = 256
	requestHeaderValueLimit  = 2048
	requestHeaderValuesLimit = 32
	requestHeaderCountLimit  = 128
	requestHeadersLimit      = 32 << 10
)

// BillingObserver transfers protection of accepted facts to the ledger and
// releases it only after the authoritative settlement transaction commits.
type BillingObserver interface {
	ProtectBillingBatch([]string)
	CompleteBillingBatch([]string)
}

type auditAcknowledgement struct {
	accepted   bool
	done       chan struct{}
	err        error
	references int
}

type auditWaiterKey struct {
	eventID string
	keyID   uint64
}

// Service 提供请求元数据审计查询，以及有界异步批量写入。
type Service struct {
	audits               repository.AuditRepository
	logger               *slog.Logger
	pending              repository.AuditPendingStore
	wake                 chan struct{}
	space                chan struct{}
	handoff              chan struct{}
	waiterSlots          chan struct{}
	waitersMu            sync.Mutex
	waiters              map[auditWaiterKey]*auditAcknowledgement
	activeWrites         sync.WaitGroup
	workerCtx            context.Context
	cancelWorker         context.CancelFunc
	workerDone           chan struct{}
	repairPending        atomic.Bool
	repairThrough        uint64
	startErr             error
	batchSize            atomic.Int64
	flushInterval        atomic.Int64
	commitDelay          atomic.Int64
	configChanged        chan struct{}
	lifecycleMu          sync.Mutex
	startOnce            sync.Once
	stopOnce             sync.Once
	stop                 chan struct{}
	done                 chan struct{}
	stopped              atomic.Bool
	started              atomic.Bool
	dropped              atomic.Uint64
	now                  func() time.Time
	summaryCache         *resultcache.Cache[string, SummaryResult]
	ledgerMu             sync.Mutex
	ledgerConfig         LedgerConfig
	ledgerFailures       int
	ledgerUnhealthySince time.Time
	ledgerLastSuccess    time.Time
	ledgerLastFailure    time.Time
	ledgerLastDrop       time.Time
	ledgerQueueHighSince time.Time
	ledgerLastWarning    time.Time
	observerMu           sync.RWMutex
	billingObserver      BillingObserver
}

func NewService(audits repository.AuditRepository, pending repository.AuditPendingStore, logger *slog.Logger, batchSize int, flushInterval time.Duration) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	capacity := 1
	if pending != nil {
		capacity = max(1, pending.Snapshot().MaxRecords)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	service := &Service{
		audits: audits, pending: pending, logger: logger,
		wake: make(chan struct{}, 1), space: make(chan struct{}), handoff: make(chan struct{}, 1),
		waiterSlots: make(chan struct{}, capacity), waiters: make(map[auditWaiterKey]*auditAcknowledgement),
		workerCtx: workerCtx, cancelWorker: cancelWorker, workerDone: make(chan struct{}),
		configChanged: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		now: time.Now, summaryCache: resultcache.New[string, SummaryResult](64, auditSummaryTTL), ledgerConfig: defaultLedgerConfig(),
	}
	service.UpdateConfig(batchSize, flushInterval)
	return service
}

func defaultLedgerConfig() LedgerConfig {
	return LedgerConfig{
		Mode:                      LedgerModeEnforce,
		FailureThreshold:          1,
		UnhealthyGrace:            10 * time.Second,
		QueueHighWatermarkPercent: 90,
	}
}

// UpdateLedgerConfig changes readiness policy without altering the audit writer.
func (s *Service) UpdateLedgerConfig(value LedgerConfig) {
	defaults := defaultLedgerConfig()
	if value.Mode != LedgerModeObserve && value.Mode != LedgerModeEnforce {
		value.Mode = defaults.Mode
	}
	if value.FailureThreshold <= 0 {
		value.FailureThreshold = defaults.FailureThreshold
	}
	if value.UnhealthyGrace <= 0 {
		value.UnhealthyGrace = defaults.UnhealthyGrace
	}
	if value.QueueHighWatermarkPercent < 1 || value.QueueHighWatermarkPercent > 100 {
		value.QueueHighWatermarkPercent = defaults.QueueHighWatermarkPercent
	}
	s.ledgerMu.Lock()
	s.ledgerConfig = value
	s.ledgerMu.Unlock()
}

// SetBillingObserver is wired before Start so recovered pending facts are
// protected before cleanup workers or request traffic can run.
func (s *Service) SetBillingObserver(observer BillingObserver) {
	s.observerMu.Lock()
	s.billingObserver = observer
	s.observerMu.Unlock()
}

// LedgerSnapshot returns a bounded, identity-free view of the durable ledger state.
func (s *Service) LedgerSnapshot() LedgerSnapshot {
	now := s.now().UTC()
	state := repository.AuditPendingSnapshot{}
	if s.pending != nil {
		state = s.pending.Snapshot()
	}
	queueDepth, queueCapacity := state.Records, state.MaxRecords

	s.ledgerMu.Lock()
	s.updateQueuePressureLocked(now, queueDepth, queueCapacity, state.Bytes, state.MaxBytes)
	dropped := s.dropped.Load()
	ready := dropped == 0 && state.Rejected == 0 && !s.repairPending.Load() && s.ledgerReadyLocked(now)
	snapshot := LedgerSnapshot{
		Mode:          s.ledgerConfig.Mode,
		Ready:         ready,
		Irrecoverable: dropped > 0,
		QueueDepth:    queueDepth,
		RepairPending: s.repairPending.Load(),
		Rejected:      state.Rejected, PendingBytes: state.Bytes, CapacityBytes: state.MaxBytes,
		QueueCapacity:       queueCapacity,
		ConsecutiveFailures: s.ledgerFailures,
		Dropped:             dropped,
		LastSuccessAt:       s.ledgerLastSuccess,
		LastFailureAt:       s.ledgerLastFailure,
		UnhealthySince:      s.ledgerUnhealthySince,
	}
	s.ledgerMu.Unlock()

	perfmetrics.Default.SetGauge("audit_queue_depth", perfmetrics.Labels{Subsystem: "audit", Stage: "queue"}, int64(queueDepth))
	perfmetrics.Default.SetGauge("audit_queue_capacity", perfmetrics.Labels{Subsystem: "audit", Stage: "queue"}, int64(queueCapacity))
	return snapshot
}

// CheckLedgerReady blocks unaccepted facts and retained invalid records even in
// observe mode. Recoverable writer/queue degradation follows the configured grace.
func (s *Service) CheckLedgerReady() error {
	snapshot := s.LedgerSnapshot()
	if snapshot.Ready && !snapshot.Irrecoverable {
		return nil
	}
	if !snapshot.Irrecoverable && snapshot.Rejected == 0 && !snapshot.RepairPending && snapshot.Mode != LedgerModeEnforce {
		return nil
	}
	return ErrLedgerUnavailable
}

func (s *Service) UpdateConfig(batchSize int, flushInterval time.Duration) {
	s.UpdateWriterConfig(batchSize, flushInterval, time.Duration(s.commitDelay.Load()))
}

// UpdateWriterConfig hot-reloads batching limits without replacing the active queue.
func (s *Service) UpdateWriterConfig(batchSize int, flushInterval, commitDelay time.Duration) {
	if commitDelay <= 0 {
		commitDelay = auditDefaultCommitDelay
	}
	batchSize = max(1, min(batchSize, 1000))
	if flushInterval <= 0 {
		flushInterval = time.Second
	}
	s.batchSize.Store(int64(batchSize))
	s.flushInterval.Store(int64(flushInterval))
	s.commitDelay.Store(int64(commitDelay))
	select {
	case s.configChanged <- struct{}{}:
	default:
	}
}

// Start restores pending reservation protection before making the writer available.
func (s *Service) Start(ctx context.Context) error {
	s.startOnce.Do(func() {
		s.lifecycleMu.Lock()
		if s.stopped.Load() {
			s.lifecycleMu.Unlock()
			s.startErr = ErrWriterUnavailable
			return
		}
		s.activeWrites.Add(1)
		s.lifecycleMu.Unlock()
		defer s.activeWrites.Done()
		if s.pending == nil {
			s.startErr = errors.New("audit writer requires durable pending storage")
			return
		}
		startupCtx, cancel := context.WithCancel(ctx)
		stopCancel := context.AfterFunc(s.workerCtx, cancel)
		defer cancel()
		defer stopCancel()
		var after uint64
		for {
			entries, err := s.pending.PendingEventIDs(startupCtx, after, 500)
			if err != nil {
				s.startErr = err
				return
			}
			if len(entries) == 0 {
				break
			}
			s.observeBilling(entries, true)
			for _, entry := range entries {
				if entry.Rejected {
					s.repairPending.Store(true)
				}
			}
			after = entries[len(entries)-1].ID
		}
		s.repairThrough = after
		if err := s.pending.RetryRejected(startupCtx); err != nil {
			s.startErr = err
			return
		}
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		if s.stopped.Load() {
			s.startErr = ErrWriterUnavailable
			return
		}
		s.started.Store(true)
		go s.runSupervised()
	})
	return s.startErr
}

// Create durably accepts a sanitized fact before waiting for its SQL settlement.
// A cancelled caller abandons only its acknowledgement; the fact is retained.
// Success still means the authoritative audit and billing transaction committed.
func (s *Service) Create(ctx context.Context, value auditdomain.Record) error {
	startedAt := time.Now()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.lifecycleMu.Lock()
	if !s.started.Load() || s.stopped.Load() {
		s.lifecycleMu.Unlock()
		return ErrWriterUnavailable
	}
	s.activeWrites.Add(1)
	s.lifecycleMu.Unlock()
	defer s.activeWrites.Done()
	writeCtx, cancel := context.WithCancel(ctx)
	stopCancel := context.AfterFunc(s.workerCtx, cancel)
	defer cancel()
	defer stopCancel()
	if value.ClientIP == "" {
		value.ClientIP = requestmeta.ClientIP(ctx)
	}
	value = sanitizeRequestMetadata(value)
	value.EventID = value.EventIdentity()
	key := auditWaiterKey{value.EventID, value.ClientKeyID}
	ack, err := s.registerWaiter(writeCtx, key)
	if err != nil {
		s.recordUnaccepted(value, err)
		return err
	}
	defer s.releaseWaiter(key, ack)
	for {
		entry, space, err := s.appendPending(writeCtx, value)
		if err == nil {
			if entry.Rejected {
				return fmt.Errorf("%w: retained audit requires repair", repository.ErrInvalidRecord)
			}
			break
		}
		if !errors.Is(err, repository.ErrAuditPendingFull) {
			s.recordUnaccepted(value, err)
			return err
		}
		select {
		case <-space:
		case <-writeCtx.Done():
			s.recordUnaccepted(value, writeCtx.Err())
			return writeCtx.Err()
		}
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	err = nil
	outcome := "success"
	select {
	case <-ack.done:
		err = ack.err
		if err != nil {
			outcome = "failed"
		}
	case <-ctx.Done():
		err = ctx.Err()
		outcome = "timeout"
	case <-s.stop:
		err = ErrWriterUnavailable
		outcome = "stopped"
	}
	labels := perfmetrics.Labels{Subsystem: "audit", Operation: string(value.Operation), Stage: "ack", Outcome: outcome}
	perfmetrics.Default.Inc("audit_records_total", labels)
	perfmetrics.Default.ObserveDuration("audit_ack_duration_us", labels, time.Since(startedAt))
	return err
}

func sanitizeRequestMetadata(value auditdomain.Record) auditdomain.Record {
	// Query strings can contain credentials or signed resource URLs. Endpoint
	// identity only needs the path.
	value.RequestMethod = truncateRequestMetadata(strings.ToUpper(strings.TrimSpace(value.RequestMethod)), requestMethodLimit)
	value.RequestPath = truncateRequestMetadata(strings.SplitN(value.RequestPath, "?", 2)[0], requestPathLimit)
	if len(value.RequestHeaders) == 0 {
		value.RequestHeaders = nil
		return value
	}
	names := make([]string, 0, len(value.RequestHeaders))
	for name := range value.RequestHeaders {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make(map[string][]string, len(value.RequestHeaders))
	for _, originalName := range names {
		if len(result) >= requestHeaderCountLimit {
			break
		}
		name := truncateRequestMetadata(strings.TrimSpace(originalName), requestHeaderNameLimit)
		if name == "" {
			continue
		}
		values := value.RequestHeaders[originalName]
		cloned := make([]string, 0, len(values))
		if isSensitiveRequestHeader(name) {
			cloned = []string{"[REDACTED]"}
		} else {
			for _, item := range values[:min(len(values), requestHeaderValuesLimit)] {
				cloned = append(cloned, truncateRequestMetadata(item, requestHeaderValueLimit))
			}
		}
		result[name] = cloned
		encoded, err := json.Marshal(result)
		if err != nil || len(encoded) > requestHeadersLimit {
			delete(result, name)
			break
		}
	}
	value.RequestHeaders = result
	return value
}

func truncateRequestMetadata(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func isSensitiveRequestHeader(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{
		"auth", "cookie", "credential", "dpop", "jwt", "key", "password", "secret", "session", "signature", "token",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// Close stops admission and cancels SQL work. Pending facts remain in the
// journal. A nil result guarantees that both the worker and appenders stopped.
func (s *Service) Close(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.stopped.Store(true)
		close(s.stop)
		s.cancelWorker()
		s.lifecycleMu.Unlock()
		go func() {
			s.activeWrites.Wait()
			if s.started.Load() {
				<-s.workerDone
			}
			close(s.done)
		}()
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) List(ctx context.Context, page, pageSize int) ([]auditdomain.Record, int64, error) {
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	return s.audits.List(ctx, (page-1)*pageSize, pageSize)
}

func (s *Service) Get(ctx context.Context, id uint64) (auditdomain.Record, error) {
	return s.audits.Get(ctx, id)
}

// CursorResult 表示按递减 ID 游标读取的一页审计记录。
type CursorResult struct {
	Items      []auditdomain.Record
	NextCursor string
	HasMore    bool
}

type ListFilter struct {
	Model     string
	Status    string
	Mode      string
	Key       string
	Account   string
	ErrorCode string
	Sort      repository.SortQuery
}

type auditCursorPayload struct {
	Version   int                      `json:"v"`
	Field     string                   `json:"field"`
	Direction repository.SortDirection `json:"direction"`
	ID        uint64                   `json:"id"`
	Value     string                   `json:"value"`
}

// ListCursor 使用复合游标读取审计，适合持续增长且支持多字段排序的大数据列表。
func (s *Service) ListCursor(ctx context.Context, rawCursor string, pageSize int, search, rawPeriod string, filter ListFilter) (CursorResult, error) {
	_, pageSize = repository.NormalizePage(1, pageSize, repository.DefaultCursorPageSize)
	if filter.Sort.Field == "" && filter.Sort.Direction == "" {
		filter.Sort = repository.SortQuery{Field: "createdAt", Direction: repository.SortDescending}
	}
	if !validAuditFilter(filter.Status, "", "success", "clientError", "serverError", "2xx", "4xx", "5xx", "other") || !validAuditFilter(filter.Mode, "", "stream", "nonStream") || !repository.IsValidSort(filter.Sort, "request", "model", "billing", "tokens", "status", "mode", "duration", "createdAt") {
		return CursorResult{}, ErrInvalidFilter
	}
	cursor, err := decodeAuditCursor(rawCursor, filter.Sort)
	if err != nil {
		return CursorResult{}, err
	}
	_, start, end, err := s.resolvePeriod(rawPeriod)
	if err != nil {
		return CursorResult{}, err
	}
	items, hasMore, err := s.audits.ListCursor(ctx, repository.AuditCursorQuery{Cursor: cursor, Limit: pageSize, Search: search, Start: start, End: end, Sort: filter.Sort, Filter: repository.AuditListFilter{
		Model: filter.Model, Status: filter.Status, Mode: filter.Mode, Key: filter.Key, Account: filter.Account, ErrorCode: filter.ErrorCode,
	}})
	if err != nil {
		return CursorResult{}, err
	}
	result := CursorResult{Items: items, HasMore: hasMore}
	if hasMore && len(items) > 0 {
		result.NextCursor, err = encodeAuditCursor(items[len(items)-1], filter.Sort)
		if err != nil {
			return CursorResult{}, err
		}
	}
	return result, nil
}

func decodeAuditCursor(raw string, sort repository.SortQuery) (*repository.SortCursor, error) {
	if raw == "" {
		return nil, nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var payload auditCursorPayload
	if json.Unmarshal(encoded, &payload) != nil || payload.Version != 1 || payload.ID == 0 || payload.Field != sort.Field || payload.Direction != sort.Direction {
		return nil, ErrInvalidCursor
	}
	value, err := parseAuditCursorValue(payload.Field, payload.Value)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	return &repository.SortCursor{ID: payload.ID, Value: value}, nil
}

func encodeAuditCursor(value auditdomain.Record, sort repository.SortQuery) (string, error) {
	payload := auditCursorPayload{Version: 1, Field: sort.Field, Direction: sort.Direction, ID: value.ID, Value: formatAuditCursorValue(value, sort.Field)}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func parseAuditCursorValue(field, value string) (any, error) {
	switch field {
	case "request", "model":
		return value, nil
	case "billing", "tokens", "status", "mode", "duration":
		return strconv.ParseInt(value, 10, 64)
	case "createdAt":
		return time.Parse(time.RFC3339Nano, value)
	default:
		return nil, ErrInvalidCursor
	}
}

func formatAuditCursorValue(value auditdomain.Record, field string) string {
	switch field {
	case "request":
		return value.RequestID
	case "model":
		return strings.ToLower(value.ModelPublicID)
	case "billing":
		amount := value.CostInUSDTicks
		if amount == 0 {
			amount = value.EstimatedCostInUSDTicks
		}
		return strconv.FormatInt(amount, 10)
	case "tokens":
		return strconv.FormatInt(value.TotalTokens, 10)
	case "status":
		return strconv.Itoa(value.StatusCode)
	case "mode":
		if value.Streaming {
			return "1"
		}
		return "0"
	case "duration":
		return strconv.FormatInt(value.DurationMS, 10)
	default:
		return value.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
}

type SummaryUsage struct {
	Requests                int64
	SuccessfulRequests      int64
	FailedRequests          int64
	InputTokens             int64
	CachedInputTokens       int64
	OutputTokens            int64
	ReasoningTokens         int64
	TotalTokens             int64
	AverageDurationMS       float64
	SuccessRate             float64
	EstimatedCostInUSDTicks int64
	PricedRequests          int64
	UnpricedRequests        int64
	PricedTokens            int64
	UnpricedTokens          int64
}

type SummaryResult struct {
	Period      Period
	GeneratedAt time.Time
	Start       time.Time
	End         time.Time
	Usage       SummaryUsage
}

func (s *Service) Summary(ctx context.Context, search, rawPeriod string, filter ListFilter) (SummaryResult, error) {
	return s.summary(ctx, search, rawPeriod, filter, true)
}

// SummaryFresh 绕过短缓存，供管理员显式刷新时读取最新汇总。
func (s *Service) SummaryFresh(ctx context.Context, search, rawPeriod string, filter ListFilter) (SummaryResult, error) {
	return s.summary(ctx, search, rawPeriod, filter, false)
}

func (s *Service) summary(ctx context.Context, search, rawPeriod string, filter ListFilter, useCache bool) (SummaryResult, error) {
	if !validAuditFilter(filter.Status, "", "success", "clientError", "serverError", "2xx", "4xx", "5xx", "other") || !validAuditFilter(filter.Mode, "", "stream", "nonStream") {
		return SummaryResult{}, ErrInvalidFilter
	}
	period, start, end, err := s.resolvePeriod(rawPeriod)
	if err != nil {
		return SummaryResult{}, err
	}
	if !useCache {
		return s.loadSummary(ctx, search, filter, period, start, end)
	}
	cacheKey := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", period, search, filter.Model, filter.Status, filter.Mode, filter.Key, filter.Account, filter.ErrorCode)
	return s.summaryCache.Load(ctx, cacheKey, end, func() (SummaryResult, error) {
		return s.loadSummary(ctx, search, filter, period, start, end)
	})
}

func (s *Service) loadSummary(ctx context.Context, search string, filter ListFilter, period Period, start, end time.Time) (SummaryResult, error) {
	aggregate, err := s.audits.Summarize(ctx, repository.AuditSummaryQuery{Search: search, Start: start, End: end, Filter: repository.AuditListFilter{
		Model: filter.Model, Status: filter.Status, Mode: filter.Mode, Key: filter.Key, Account: filter.Account, ErrorCode: filter.ErrorCode,
	}})
	if err != nil {
		return SummaryResult{}, err
	}
	usage := SummaryUsage{
		Requests: aggregate.Requests, SuccessfulRequests: aggregate.SuccessfulRequests, FailedRequests: aggregate.FailedRequests,
		InputTokens: aggregate.InputTokens, CachedInputTokens: aggregate.CachedInputTokens, OutputTokens: aggregate.OutputTokens,
		ReasoningTokens: aggregate.ReasoningTokens, TotalTokens: aggregate.TotalTokens,
		EstimatedCostInUSDTicks: aggregate.EstimatedCostInUSDTicks, PricedRequests: aggregate.PricedRequests,
		UnpricedRequests: aggregate.UnpricedRequests, PricedTokens: aggregate.PricedTokens, UnpricedTokens: aggregate.UnpricedTokens,
	}
	if aggregate.Requests > 0 {
		usage.SuccessRate = float64(aggregate.SuccessfulRequests) / float64(aggregate.Requests) * 100
		usage.AverageDurationMS = float64(aggregate.DurationMS) / float64(aggregate.Requests)
	}
	return SummaryResult{Period: period, GeneratedAt: end, Start: start, End: end, Usage: usage}, nil
}

func (s *Service) resolvePeriod(value string) (Period, time.Time, time.Time, error) {
	period, duration, err := parsePeriod(value)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	end := s.now().UTC()
	return period, end.Add(-duration), end, nil
}

func parsePeriod(value string) (Period, time.Duration, error) {
	if value == "" {
		value = string(Period24Hours)
	}
	switch Period(value) {
	case Period24Hours:
		return Period24Hours, 24 * time.Hour, nil
	case Period7Days:
		return Period7Days, 7 * 24 * time.Hour, nil
	case Period30Days:
		return Period30Days, 30 * 24 * time.Hour, nil
	case Period90Days:
		return Period90Days, 90 * 24 * time.Hour, nil
	default:
		return "", 0, ErrInvalidPeriod
	}
}

func validAuditFilter(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (s *Service) runSupervised() {
	defer close(s.workerDone)
	delay := auditWriteRetryBase
	for s.workerCtx.Err() == nil {
		err := batch.Do(s.workerCtx, func(ctx context.Context) error { return s.run(ctx) })
		if s.workerCtx.Err() != nil {
			return
		}
		if err != nil {
			s.recordLedgerFailure()
			s.logger.Error("audit_worker_restarting", "error", err)
		}
		if !s.waitWorker(delay) {
			return
		}
		delay = min(delay*2, auditWriteRetryMax)
	}
}

func (s *Service) waitWorker(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-s.workerCtx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) run(ctx context.Context) error {
	for ctx.Err() == nil {
		// One short delay coalesces newly accepted facts. Recovery batches at the
		// size limit continue immediately; flushInterval bounds a missed wakeup.
		select {
		case <-s.wake:
		case <-s.configChanged:
		default:
			if s.pending.Snapshot().Records <= s.pending.Snapshot().Rejected {
				timer := time.NewTimer(time.Duration(s.flushInterval.Load()))
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-s.wake:
					timer.Stop()
				case <-s.configChanged:
					timer.Stop()
				case <-timer.C:
				}
			}
		}
		if !s.waitWorker(time.Duration(s.commitDelay.Load())) {
			return ctx.Err()
		}
		for {
			entries, err := s.pending.ReadPending(ctx, 0, int(s.batchSize.Load()))
			if err != nil {
				return err
			}
			if len(entries) == 0 || entries[0].ID > s.repairThrough {
				s.repairPending.Store(false)
			}
			if len(entries) == 0 {
				break
			}
			if err := s.persistBatch(ctx, entries); err != nil {
				return err
			}
			if len(entries) < int(s.batchSize.Load()) {
				break
			}
		}
	}
	return ctx.Err()
}

func (s *Service) persistBatch(ctx context.Context, entries []repository.AuditPendingEntry) error {
	startedAt := time.Now()
	retryDelay := auditWriteRetryBase
	for len(entries) > 0 && ctx.Err() == nil {
		for i := 0; i < len(entries); {
			if entries[i].DecodeError == nil {
				i++
				continue
			}
			if err := s.rejectEntry(ctx, entries[i], entries[i].DecodeError); err != nil {
				return err
			}
			entries = append(entries[:i], entries[i+1:]...)
		}
		if len(entries) == 0 {
			return nil
		}
		records := make([]auditdomain.Record, len(entries))
		ids := make([]uint64, len(entries))
		for i, entry := range entries {
			records[i] = entry.Record
			ids[i] = entry.ID
		}
		workCtx, cancel := context.WithTimeout(ctx, auditWriteTimeout)
		err := batch.Do(workCtx, func(workCtx context.Context) error { return s.audits.CreateBatch(workCtx, records) })
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var invalid *repository.InvalidBatchRecordError
		if errors.As(err, &invalid) && invalid.Index >= 0 && invalid.Index < len(entries) {
			if err := s.rejectEntry(ctx, entries[invalid.Index], err); err != nil {
				return err
			}
			entries = append(entries[:invalid.Index], entries[invalid.Index+1:]...)
			continue
		}
		if err == nil {
			// Coordinate journal deletion and protection with Append. SQL can run
			// independently; a replay after deletion failure is safe by EventID.
			ackErr := s.acknowledgePending(ctx, entries, ids)
			if ackErr != nil {
				return ackErr
			}
			s.recordLedgerSuccess()
			perfmetrics.Default.Add("audit_records_total", perfmetrics.Labels{Subsystem: "audit", Stage: "batch", Outcome: "success"}, int64(len(records)))
			perfmetrics.Default.ObserveDuration("audit_batch_commit_duration_us", perfmetrics.Labels{Subsystem: "audit", Stage: "batch", Outcome: "success"}, time.Since(startedAt))
			return nil
		}
		s.recordLedgerFailure()
		s.logger.Warn("audit_batch_write_retrying", "count", len(entries), "retry_in", retryDelay, "error", err)
		if !s.waitWorker(retryDelay) {
			return ctx.Err()
		}
		retryDelay = min(retryDelay*2, auditWriteRetryMax)
	}
	return ctx.Err()
}

func (s *Service) appendPending(ctx context.Context, value auditdomain.Record) (repository.AuditPendingEntry, <-chan struct{}, error) {
	// An accepted duplicate needs only the shared SQL acknowledgement.
	key := auditWaiterKey{value.EventID, value.ClientKeyID}
	s.waitersMu.Lock()
	accepted := s.waiters[key] != nil && s.waiters[key].accepted
	s.waitersMu.Unlock()
	if accepted {
		return repository.AuditPendingEntry{Record: value}, nil, nil
	}
	select {
	case s.handoff <- struct{}{}:
	case <-ctx.Done():
		return repository.AuditPendingEntry{}, nil, ctx.Err()
	}
	defer func() { <-s.handoff }()
	s.waitersMu.Lock()
	space := s.space
	accepted = s.waiters[key] != nil && s.waiters[key].accepted
	s.waitersMu.Unlock()
	if accepted {
		return repository.AuditPendingEntry{Record: value}, space, nil
	}
	entry, err := s.pending.Append(ctx, value)
	if err == nil {
		s.observeBilling([]repository.AuditPendingEntry{entry}, true)
		s.waitersMu.Lock()
		if ack := s.waiters[key]; ack != nil {
			ack.accepted = true
		}
		s.waitersMu.Unlock()
		if entry.Rejected {
			s.completeWrites([]repository.AuditPendingEntry{entry}, fmt.Errorf("%w: retained audit requires repair", repository.ErrInvalidRecord))
		}
	}
	return entry, space, err
}

func (s *Service) acknowledgePending(ctx context.Context, entries []repository.AuditPendingEntry, ids []uint64) error {
	select {
	case s.handoff <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.handoff }()
	err := s.pending.Acknowledge(ctx, ids)
	s.observeBilling(entries, false)
	s.completeWrites(entries, nil)
	return err
}

func (s *Service) rejectEntry(ctx context.Context, entry repository.AuditPendingEntry, cause error) error {
	if err := s.pending.Reject(ctx, entry.ID, cause.Error()); err != nil {
		return err
	}
	s.completeWrites([]repository.AuditPendingEntry{entry}, cause)
	s.logger.Error("audit_record_retained_for_repair", "event_id", entry.Record.EventID, "error", cause)
	return nil
}

// Duplicate callers share one acknowledgement. Only distinct in-flight event
// identities consume the bounded waiter slots; retrying an already accepted
// event cannot be mistaken for a new fact rejected by a full queue.
func (s *Service) registerWaiter(ctx context.Context, key auditWaiterKey) (*auditAcknowledgement, error) {
	for {
		s.waitersMu.Lock()
		if ack := s.waiters[key]; ack != nil {
			ack.references++
			s.waitersMu.Unlock()
			return ack, nil
		}
		select {
		case s.waiterSlots <- struct{}{}:
			ack := &auditAcknowledgement{done: make(chan struct{}), references: 1}
			s.waiters[key] = ack
			s.waitersMu.Unlock()
			return ack, nil
		default:
			space := s.space
			s.waitersMu.Unlock()
			select {
			case <-space:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
}

func (s *Service) releaseWaiter(key auditWaiterKey, ack *auditAcknowledgement) {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()
	ack.references--
	if ack.references == 0 {
		delete(s.waiters, key)
		<-s.waiterSlots
		s.notifySpaceLocked()
	}
}

func (s *Service) completeWrites(entries []repository.AuditPendingEntry, err error) {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()
	for _, entry := range entries {
		if ack := s.waiters[auditWaiterKey{entry.Record.EventID, entry.Record.ClientKeyID}]; ack != nil {
			select {
			case <-ack.done:
			default:
				ack.err = err
				close(ack.done)
			}
		}
	}
	s.notifySpaceLocked()
}

func (s *Service) notifySpaceLocked() { close(s.space); s.space = make(chan struct{}) }

func (s *Service) recordUnaccepted(value auditdomain.Record, err error) {
	s.dropped.Add(1)
	s.recordLedgerDrop()
	s.logger.Error("audit_fact_not_accepted", "event_id", value.EventID, "error", err)
	// Do not remove protection for an earlier accepted copy of the same event.
	// M08's request-side ownership is released explicitly by its caller.
}

func (s *Service) recordLedgerSuccess() {
	if s.pending.Snapshot().Records == 0 {
		s.repairPending.Store(false)
	}
	now := s.now().UTC()
	s.ledgerMu.Lock()
	s.ledgerFailures = 0
	s.ledgerLastSuccess = now
	if s.dropped.Load() == 0 && s.ledgerQueueHighSince.IsZero() {
		s.ledgerUnhealthySince = time.Time{}
	}
	s.ledgerMu.Unlock()
}

func (s *Service) recordLedgerFailure() {
	now := s.now().UTC()
	s.ledgerMu.Lock()
	s.ledgerFailures++
	s.ledgerLastFailure = now
	if s.ledgerFailures >= s.ledgerConfig.FailureThreshold && s.ledgerUnhealthySince.IsZero() {
		s.ledgerUnhealthySince = now
	}
	s.warnLedgerIfNeededLocked(now, "durable_write_failed")
	s.ledgerMu.Unlock()
}

func (s *Service) recordLedgerDrop() {
	now := s.now().UTC()
	s.ledgerMu.Lock()
	s.ledgerLastDrop = now
	if s.ledgerUnhealthySince.IsZero() {
		s.ledgerUnhealthySince = now
	}
	s.warnLedgerIfNeededLocked(now, "audit_record_dropped")
	s.ledgerMu.Unlock()
}

func (s *Service) updateQueuePressureLocked(now time.Time, depth, capacity int, size, maxSize int64) {
	recordsHigh := capacity > 0 && depth*100 >= capacity*s.ledgerConfig.QueueHighWatermarkPercent
	bytesHigh := maxSize > 0 && size*100 >= maxSize*int64(s.ledgerConfig.QueueHighWatermarkPercent)
	if !recordsHigh && !bytesHigh {
		wasHigh := !s.ledgerQueueHighSince.IsZero()
		s.ledgerQueueHighSince = time.Time{}
		if wasHigh && s.ledgerFailures == 0 && s.dropped.Load() == 0 {
			s.ledgerUnhealthySince = time.Time{}
		}
		return
	}
	if s.ledgerQueueHighSince.IsZero() {
		s.ledgerQueueHighSince = now
	}
	if s.ledgerUnhealthySince.IsZero() {
		s.ledgerUnhealthySince = s.ledgerQueueHighSince
	}
}

func (s *Service) ledgerReadyLocked(now time.Time) bool {
	degradedSince := s.ledgerUnhealthySince
	if !s.ledgerQueueHighSince.IsZero() && (degradedSince.IsZero() || s.ledgerQueueHighSince.Before(degradedSince)) {
		degradedSince = s.ledgerQueueHighSince
	}
	if degradedSince.IsZero() {
		return true
	}
	return now.Sub(degradedSince) < s.ledgerConfig.UnhealthyGrace
}

func (s *Service) warnLedgerIfNeededLocked(now time.Time, reason string) {
	if !s.ledgerLastWarning.IsZero() && now.Sub(s.ledgerLastWarning) < time.Minute {
		return
	}
	s.ledgerLastWarning = now
	s.logger.Warn("billing_ledger_degraded", "reason", reason, "mode", s.ledgerConfig.Mode, "consecutive_failures", s.ledgerFailures, "pending", s.pending.Snapshot().Records, "capacity", s.pending.Snapshot().MaxRecords)
}

func (s *Service) observeBilling(entries []repository.AuditPendingEntry, protect bool) {
	s.observerMu.RLock()
	observer := s.billingObserver
	s.observerMu.RUnlock()
	if observer == nil {
		return
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Record.EventID != "" {
			ids = append(ids, entry.Record.EventID)
		}
	}
	if protect {
		observer.ProtectBillingBatch(ids)
	} else {
		observer.CompleteBillingBatch(ids)
	}
}
