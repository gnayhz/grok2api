package redis

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

const (
	concurrencyLeaseGrace                = time.Minute
	concurrencyReleaseRetryInterval      = 250 * time.Millisecond
	concurrencyReleaseRetryTimeout       = 3 * time.Second
	concurrencyReleaseRetryBatchSize     = 512
	concurrencyReleaseRetryQueueCapacity = 16384
	maxStickyBindingsPerAccount          = 10000
	maxDeviceSessions                    = 1000
	maxQuotaRecoveryEvents               = 100000
	maxQuotaRefreshDirty                 = 100000
	observedModelStateTTL                = 30 * time.Minute
	// At the per-account cap, one pipeline processes at most 80,000 members.
	stickyDeletePipelineSize = 8
)

var rateScript = redisclient.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
if current > tonumber(ARGV[1]) then
  local pttl = redis.call('PTTL', KEYS[1])
  if pttl < 0 then pttl = ARGV[2] end
  return {0, pttl}
end
return {1, 0}
`)

var acquireLeaseScript = redisclient.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[4])
-- A short background lease must never shorten an active production lease.
if redis.call('PTTL', KEYS[1]) < tonumber(ARGV[5]) then redis.call('PEXPIRE', KEYS[1], ARGV[5]) end
return 1
`)

var releaseLeaseScript = redisclient.NewScript(`return redis.call('ZREM', KEYS[1], ARGV[1])`)

var releaseLockScript = redisclient.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

var setStickyScript = redisclient.NewScript(`
local old = redis.call('GET', KEYS[1])
if old and old ~= ARGV[1] then redis.call('ZREM', ARGV[3] .. old, KEYS[1]) end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[4])
redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
local excess = redis.call('ZCARD', KEYS[2]) - tonumber(ARGV[6])
if excess > 0 then
  local stale = redis.call('ZRANGE', KEYS[2], 0, excess - 1)
  for _, key in ipairs(stale) do
    if redis.call('GET', key) == ARGV[1] then redis.call('DEL', key) end
    redis.call('ZREM', KEYS[2], key)
  end
end
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[2]) then redis.call('PEXPIRE', KEYS[2], ARGV[2]) end
return 1
`)

var bindStickyScript = redisclient.NewScript(`
local selected = redis.call('GET', KEYS[1])
if not selected then selected = ARGV[1] end
local accountSetKey = ARGV[3] .. selected
redis.call('SET', KEYS[1], selected, 'PX', ARGV[2])
redis.call('ZREMRANGEBYSCORE', accountSetKey, '-inf', ARGV[4])
redis.call('ZADD', accountSetKey, ARGV[5], KEYS[1])
local excess = redis.call('ZCARD', accountSetKey) - tonumber(ARGV[6])
if excess > 0 then
  local stale = redis.call('ZRANGE', accountSetKey, 0, excess - 1)
  for _, key in ipairs(stale) do
    if redis.call('GET', key) == selected then redis.call('DEL', key) end
    redis.call('ZREM', accountSetKey, key)
  end
end
if redis.call('PTTL', accountSetKey) < tonumber(ARGV[2]) then redis.call('PEXPIRE', accountSetKey, ARGV[2]) end
return selected
`)

var deleteStickyByAccountScript = redisclient.NewScript(`
local members = redis.call('ZRANGE', KEYS[1], 0, -1)
local deleted = 0
for _, key in ipairs(members) do
  if redis.call('GET', key) == ARGV[1] then
    deleted = deleted + redis.call('DEL', key)
  end
end
redis.call('DEL', KEYS[1])
return deleted
`)

var createDeviceSessionScript = redisclient.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[2])
if redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[4]) then return 0 end
if not redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3], 'NX') then return -1 end
redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[3]) then redis.call('PEXPIRE', KEYS[2], ARGV[3]) end
return 1
`)

// Compare the complete payload while applying the same Go domain transition
// as the memory adapter; Lua owns only the atomic storage mechanism.
var compareDeviceSessionScript = redisclient.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
if ARGV[2] == 'delete' then
  redis.call('DEL', KEYS[1])
  redis.call('ZREM', KEYS[2], KEYS[1])
else
  redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4], 'XX')
  redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
  if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[4]) then redis.call('PEXPIRE', KEYS[2], ARGV[4]) end
end
return 1
`)

var scheduleQuotaRecoveryScript = redisclient.NewScript(`
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) and redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[4]) then return 0 end
if redis.call('HEXISTS', KEYS[3], ARGV[1]) == 1 then return 2 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
redis.call('HDEL', KEYS[3], ARGV[1])
return 1
`)

var ensureQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then return 2 end
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[4]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
return 1
`)

var cancelQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('HEXISTS', KEYS[3], ARGV[1]) == 1 then return 2 end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
return redis.call('ZREM', KEYS[1], ARGV[1])
`)

var claimQuotaRecoveryScript = redisclient.NewScript(`
local values = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
local result = {}
for _, value in ipairs(values) do
  redis.call('ZADD', KEYS[1], ARGV[3], value)
  redis.call('HSET', KEYS[3], value, ARGV[4])
  table.insert(result, value)
  table.insert(result, redis.call('HGET', KEYS[2], value) or '0')
  table.insert(result, ARGV[4])
end
return result
`)

var ackQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('HGET', KEYS[3], ARGV[1]) ~= ARGV[2] then return 0 end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
return redis.call('ZREM', KEYS[1], ARGV[1])
`)

var markQuotaRefreshDirtyScript = redisclient.NewScript(`
local now = tonumber(ARGV[4])
local expired = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', now, 'LIMIT', 0, 1000)
for _, member in ipairs(expired) do
  redis.call('ZREM', KEYS[3], member)
  redis.call('ZREM', KEYS[2], member)
  redis.call('HDEL', KEYS[1], member)
end
local memberExpires = redis.call('ZSCORE', KEYS[3], ARGV[1])
if memberExpires and tonumber(memberExpires) <= now then
  redis.call('ZREM', KEYS[3], ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('HDEL', KEYS[1], ARGV[1])
end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
if not redis.call('ZSCORE', KEYS[2], ARGV[1]) and redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[3]) then return 0 end
local generation = redis.call('HINCRBY', KEYS[1], ARGV[1], 1)
redis.call('ZADD', KEYS[2], ARGV[2], ARGV[1])
redis.call('ZADD', KEYS[3], ARGV[2], ARGV[1])
local latest = redis.call('ZREVRANGE', KEYS[3], 0, 0, 'WITHSCORES')
if #latest == 2 then
  redis.call('PEXPIREAT', KEYS[1], latest[2])
  redis.call('PEXPIREAT', KEYS[2], latest[2])
  redis.call('PEXPIREAT', KEYS[3], latest[2])
end
return generation
`)

var clearQuotaRefreshDirtyScript = redisclient.NewScript(`
local retentionExpires = redis.call('ZSCORE', KEYS[3], ARGV[1])
if not retentionExpires or tonumber(retentionExpires) <= tonumber(ARGV[3]) then
  redis.call('ZREM', KEYS[3], ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('HDEL', KEYS[1], ARGV[1])
  return 0
end
local dirtyExpires = redis.call('ZSCORE', KEYS[2], ARGV[1])
if not dirtyExpires or tonumber(dirtyExpires) <= tonumber(ARGV[3]) then
  redis.call('ZREM', KEYS[2], ARGV[1])
  return 0
end
if tonumber(retentionExpires) ~= tonumber(ARGV[4]) then return 0 end
if tonumber(redis.call('HGET', KEYS[1], ARGV[1]) or '0') ~= tonumber(ARGV[2]) then return 0 end
redis.call('ZREM', KEYS[2], ARGV[1])
return 1
`)

var scanQuotaRefreshDirtyScript = redisclient.NewScript(`
local limit = tonumber(ARGV[2])
local now = tonumber(ARGV[1])
local expired = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', now, 'LIMIT', 0, 1000)
for _, member in ipairs(expired) do
  redis.call('ZREM', KEYS[3], member)
  redis.call('ZREM', KEYS[2], member)
  redis.call('HDEL', KEYS[1], member)
end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
local cursor = tonumber(ARGV[3])
local members = redis.call('ZRANGE', KEYS[2], cursor, cursor + limit - 1)
local next = cursor + #members
if next >= redis.call('ZCARD', KEYS[2]) then next = 0 end
local result = {tostring(next)}
for _, member in ipairs(members) do
  local retentionExpires = redis.call('ZSCORE', KEYS[3], member)
  local generation = redis.call('HGET', KEYS[1], member)
  if retentionExpires and tonumber(retentionExpires) > now and generation then
    table.insert(result, member)
    table.insert(result, generation)
    table.insert(result, retentionExpires)
  end
end
return result
`)

var setObservedModelStateScript = redisclient.NewScript(`
local previous = redis.call('HGET', KEYS[1], 'observed_at')
if previous and tonumber(previous) > tonumber(ARGV[2]) then return 0 end
redis.call('HSET', KEYS[1], 'model', ARGV[1], 'observed_at', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

var quotaRefreshStateScript = redisclient.NewScript(`
local generation = redis.call('HGET', KEYS[1], ARGV[1]) or '0'
local retentionExpires = redis.call('ZSCORE', KEYS[3], ARGV[1])
if not retentionExpires or tonumber(retentionExpires) <= tonumber(ARGV[2]) then
  redis.call('ZREM', KEYS[3], ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('HDEL', KEYS[1], ARGV[1])
  generation = '0'
  return {generation, '0', '0'}
end
local dirtyExpires = redis.call('ZSCORE', KEYS[2], ARGV[1])
local dirty = '0'
if dirtyExpires and tonumber(dirtyExpires) > tonumber(ARGV[2]) then
  dirty = '1'
elseif dirtyExpires then
  redis.call('ZREM', KEYS[2], ARGV[1])
end
return {generation, dirty, retentionExpires}
`)

var rescheduleQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('HGET', KEYS[3], ARGV[1]) ~= ARGV[4] then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
redis.call('HDEL', KEYS[3], ARGV[1])
return 1
`)

// Config 表示 Redis 运行态存储的启动配置。
type Config struct {
	Address          string
	Username         string
	Password         string
	Database         int
	KeyPrefix        string
	TLS              bool
	ConcurrencyLease time.Duration
}

// Store 实现多实例共享的限流、并发租约、粘滞路由、Device OAuth 会话和分布式锁。
type Store struct {
	client                  *redisclient.Client
	prefix                  string
	concurrencyLease        time.Duration
	concurrencyReleaseQueue chan concurrencyReleaseRetry
	concurrencyReleaseStop  chan struct{}
	concurrencyReleaseDone  chan struct{}
	closeOnce               sync.Once
	closeErr                error
}

type concurrencyReleaseRetry struct {
	redisKey  string
	token     string
	expiresAt time.Time
}

// Open 连接 Redis；选中的 Redis 不可用时直接返回启动错误。
func Open(ctx context.Context, cfg Config) (*Store, error) {
	options := &redisclient.Options{Addr: cfg.Address, Username: cfg.Username, Password: cfg.Password, DB: cfg.Database}
	if cfg.TLS {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := redisclient.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("连接 Redis: %w", err)
	}
	lease := cfg.ConcurrencyLease
	if lease <= 0 {
		lease = 3 * time.Hour
	}
	store := &Store{
		client: client, prefix: cfg.KeyPrefix, concurrencyLease: lease,
		concurrencyReleaseQueue: make(chan concurrencyReleaseRetry, concurrencyReleaseRetryQueueCapacity),
		concurrencyReleaseStop:  make(chan struct{}), concurrencyReleaseDone: make(chan struct{}),
	}
	go store.runConcurrencyReleaseRetries()
	return store, nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.concurrencyReleaseStop)
		s.closeErr = s.client.Close()
		<-s.concurrencyReleaseDone
	})
	return s.closeErr
}

func (s *Store) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

func (s *Store) key(namespace, key string) string { return s.prefix + namespace + ":" + key }

func (s *Store) GetObservedModelState(ctx context.Context, accountID uint64) (repository.ObservedModelState, bool, error) {
	if accountID == 0 {
		return repository.ObservedModelState{}, false, nil
	}
	values, err := s.client.HMGet(ctx, s.key("observed-model", strconv.FormatUint(accountID, 10)), "model", "observed_at").Result()
	if err != nil {
		return repository.ObservedModelState{}, false, err
	}
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return repository.ObservedModelState{}, false, nil
	}
	model, ok := values[0].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return repository.ObservedModelState{}, false, nil
	}
	observedMillis, err := strconv.ParseInt(fmt.Sprint(values[1]), 10, 64)
	if err != nil || observedMillis <= 0 {
		return repository.ObservedModelState{}, false, nil
	}
	return repository.ObservedModelState{Model: model, ObservedAt: time.UnixMilli(observedMillis).UTC()}, true, nil
}

func (s *Store) SetObservedModelState(ctx context.Context, accountID uint64, value repository.ObservedModelState, ttl time.Duration) error {
	if accountID == 0 || strings.TrimSpace(value.Model) == "" || value.ObservedAt.IsZero() {
		return nil
	}
	if ttl <= 0 {
		ttl = observedModelStateTTL
	}
	return setObservedModelStateScript.Run(ctx, s.client,
		[]string{s.key("observed-model", strconv.FormatUint(accountID, 10))},
		strings.TrimSpace(value.Model), value.ObservedAt.UTC().UnixMilli(), ttl.Milliseconds()).Err()
}

// PublishSettingsChanged 发布运行设置失效通知，不在 Redis 中复制设置内容。
func (s *Store) PublishSettingsChanged(ctx context.Context) error {
	return s.client.Publish(ctx, s.key("events", "settings"), "reload").Err()
}

func (s *Store) PublishInvalidation(ctx context.Context, event repository.InvalidationEvent) error {
	if !event.Valid() {
		return errors.New("invalid invalidation event")
	}
	if event.PublishedAt.IsZero() {
		event.PublishedAt = time.Now().UTC()
	}
	revision, err := s.client.Incr(ctx, s.key("invalidation-revision", string(event.Layer())+":"+string(event.Provider))).Result()
	if err != nil {
		return err
	}
	event.Revision = uint64(revision)
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return s.client.Publish(ctx, s.key("events", "invalidation"), payload).Err()
}

func (s *Store) ListenInvalidations(ctx context.Context, handler func(context.Context, repository.InvalidationEvent) error) error {
	pubsub := s.client.Subscribe(ctx, s.key("events", "invalidation"))
	defer func() { _ = pubsub.Close() }()
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	channel := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case message, ok := <-channel:
			if !ok {
				return errors.New("Redis invalidation channel closed")
			}
			var event repository.InvalidationEvent
			if err := json.Unmarshal([]byte(message.Payload), &event); err != nil || !event.Valid() {
				continue
			}
			if err := handler(ctx, event); err != nil {
				return err
			}
		}
	}
}

// ListenSettingsChanges 监听设置变更并调用重载函数，go-redis 会在连接中断后自动重连。
func (s *Store) ListenSettingsChanges(ctx context.Context, handler func(context.Context) error) error {
	pubsub := s.client.Subscribe(ctx, s.key("events", "settings"))
	defer func() { _ = pubsub.Close() }()
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	channel := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-channel:
			if !ok {
				return errors.New("Redis 设置通知通道已关闭")
			}
			if err := handler(ctx); err != nil {
				return err
			}
		}
	}
}

func (s *Store) Allow(ctx context.Context, key string, limit int, _ time.Time) (bool, time.Duration, error) {
	if limit <= 0 {
		return true, 0, nil
	}
	result, err := rateScript.Run(ctx, s.client, []string{s.key("rate", key)}, limit, time.Minute.Milliseconds()).Int64Slice()
	if err != nil {
		return false, 0, err
	}
	if len(result) < 2 || result[0] != 1 {
		retryAfter := time.Duration(0)
		if len(result) > 1 && result[1] > 0 {
			retryAfter = time.Duration(result[1]) * time.Millisecond
			if retryAfter < time.Second {
				retryAfter = time.Second
			}
		}
		return false, retryAfter, nil
	}
	return true, 0, nil
}

func (s *Store) acquireConcurrency(ctx context.Context, key string, limit int) (func(), bool, error) {
	return s.acquireConcurrencyFor(ctx, key, limit, s.concurrencyLease)
}

func (s *Store) acquireConcurrencyFor(ctx context.Context, key string, limit int, ttl time.Duration) (func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if ttl <= 0 {
		return nil, false, errors.New("concurrency lease duration must be positive")
	}
	if limit <= 0 {
		return func() {}, true, nil
	}
	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	redisKey := s.key("concurrency", key)
	result, err := acquireLeaseScript.Run(ctx, s.client, []string{redisKey}, now.UnixMilli(), limit, expiresAt.UnixMilli(), token, (ttl + concurrencyLeaseGrace).Milliseconds()).Int()
	if err != nil || result != 1 {
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), concurrencyReleaseRetryTimeout)
			defer cancel()
			if err := releaseLeaseScript.Run(releaseCtx, s.client, []string{redisKey}, token).Err(); err != nil {
				s.enqueueConcurrencyReleaseRetry(concurrencyReleaseRetry{redisKey: redisKey, token: token, expiresAt: expiresAt})
			}
		})
	}, true, nil
}

func (s *Store) enqueueConcurrencyReleaseRetry(value concurrencyReleaseRetry) {
	select {
	case <-s.concurrencyReleaseStop:
		observeConcurrencyRelease("shutdown", 1)
		return
	default:
	}
	select {
	case s.concurrencyReleaseQueue <- value:
		observeConcurrencyRelease("queued", 1)
	case <-s.concurrencyReleaseStop:
		observeConcurrencyRelease("shutdown", 1)
	default:
		observeConcurrencyRelease("dropped", 1)
	}
}

func (s *Store) runConcurrencyReleaseRetries() {
	defer close(s.concurrencyReleaseDone)
	ticker := time.NewTicker(concurrencyReleaseRetryInterval)
	defer ticker.Stop()
	pending := make(map[string]concurrencyReleaseRetry)
	for {
		intake := s.concurrencyReleaseQueue
		if len(pending) >= concurrencyReleaseRetryQueueCapacity {
			// Bound both the intake channel and retained failed releases. The
			// next retry/expiry frees capacity before intake resumes.
			intake = nil
		}
		select {
		case value := <-intake:
			pending[value.token] = value
		case <-ticker.C:
			s.retryConcurrencyReleases(pending)
		case <-s.concurrencyReleaseStop:
			observeConcurrencyRelease("shutdown", len(pending)+len(s.concurrencyReleaseQueue))
			return
		}
	}
}

func (s *Store) retryConcurrencyReleases(pending map[string]concurrencyReleaseRetry) {
	if len(pending) == 0 {
		return
	}
	now := time.Now().UTC()
	expired := 0
	ctx, cancel := context.WithTimeout(context.Background(), concurrencyReleaseRetryTimeout)
	defer cancel()
	pipeline := s.client.Pipeline()
	type queuedRelease struct {
		token   string
		command *redisclient.IntCmd
	}
	commands := make([]queuedRelease, 0, min(len(pending), concurrencyReleaseRetryBatchSize))
	for token, value := range pending {
		if !now.Before(value.expiresAt) {
			delete(pending, token)
			expired++
			continue
		}
		commands = append(commands, queuedRelease{token: token, command: pipeline.ZRem(ctx, value.redisKey, value.token)})
		if len(commands) >= concurrencyReleaseRetryBatchSize {
			break
		}
	}
	if len(commands) == 0 {
		observeConcurrencyRelease("expired", expired)
		return
	}
	_, _ = pipeline.Exec(ctx)
	recovered := 0
	failed := 0
	for _, value := range commands {
		if value.command.Err() == nil {
			delete(pending, value.token)
			recovered++
		} else {
			failed++
		}
	}
	observeConcurrencyRelease("expired", expired)
	observeConcurrencyRelease("recovered", recovered)
	observeConcurrencyRelease("retry_failed", failed)
}

func observeConcurrencyRelease(outcome string, count int) {
	if count <= 0 {
		return
	}
	perfmetrics.Default.Add("runtime_concurrency_release_total", perfmetrics.Labels{
		Subsystem: "runtime", Operation: "concurrency_lease", Stage: "release", Outcome: outcome,
	}, int64(count))
}

func (s *Store) Current(ctx context.Context, key string) (int, error) {
	redisKey := s.key("concurrency", key)
	now := time.Now().UTC().UnixMilli()
	pipe := s.client.TxPipeline()
	pipe.ZRemRangeByScore(ctx, redisKey, "-inf", strconv.FormatInt(now, 10))
	count := pipe.ZCard(ctx, redisKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return int(count.Val()), nil
}

func (s *Store) CurrentMany(ctx context.Context, keys []string) (map[string]int, error) {
	values := make(map[string]int)
	if len(keys) == 0 {
		return values, nil
	}
	now := "(" + strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	pipe := s.client.Pipeline()
	counts := make([]*redisclient.IntCmd, len(keys))
	for index, key := range keys {
		redisKey := s.key("concurrency", key)
		counts[index] = pipe.ZCount(ctx, redisKey, now, "+inf")
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for index, count := range counts {
		if value := int(count.Val()); value > 0 {
			values[keys[index]] = value
		}
	}
	return values, nil
}

func (s *Store) Get(ctx context.Context, key string, now time.Time) (uint64, bool, error) {
	value, err := s.client.Get(ctx, s.key("sticky", key)).Result()
	if errors.Is(err, redisclient.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	id, err := strconv.ParseUint(value, 10, 64)
	return id, err == nil, err
}

func (s *Store) Bind(ctx context.Context, key string, proposedAccountID uint64, now, expiresAt time.Time) (uint64, error) {
	if key == "" || proposedAccountID == 0 {
		return proposedAccountID, nil
	}
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return proposedAccountID, nil
	}
	id := strconv.FormatUint(proposedAccountID, 10)
	value, err := bindStickyScript.Run(
		ctx,
		s.client,
		[]string{s.key("sticky", key)},
		id,
		ttl.Milliseconds(),
		s.prefix+"sticky-account:",
		now.UnixMilli(),
		expiresAt.UnixMilli(),
		maxStickyBindingsPerAccount,
	).Uint64()
	if err != nil {
		return 0, err
	}
	return value, nil
}

func (s *Store) Set(ctx context.Context, key string, accountID uint64, expiresAt time.Time) error {
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return nil
	}
	id := strconv.FormatUint(accountID, 10)
	bindingKey := s.key("sticky", key)
	accountSetPrefix := s.prefix + "sticky-account:"
	accountSetKey := accountSetPrefix + id
	now := time.Now().UTC()
	return setStickyScript.Run(ctx, s.client, []string{bindingKey, accountSetKey}, id, ttl.Milliseconds(), accountSetPrefix, now.UnixMilli(), expiresAt.UnixMilli(), maxStickyBindingsPerAccount).Err()
}

func (s *Store) DeleteByAccount(ctx context.Context, accountID uint64) error {
	id := strconv.FormatUint(accountID, 10)
	return deleteStickyByAccountScript.Run(ctx, s.client, []string{s.key("sticky-account", id)}, id).Err()
}

func (s *Store) DeleteByAccounts(ctx context.Context, accountIDs []uint64) error {
	seen := make(map[uint64]struct{}, len(accountIDs))
	ids := make([]uint64, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		if accountID == 0 {
			continue
		}
		if _, exists := seen[accountID]; exists {
			continue
		}
		seen[accountID] = struct{}{}
		ids = append(ids, accountID)
	}
	for start := 0; start < len(ids); start += stickyDeletePipelineSize {
		end := min(start+stickyDeletePipelineSize, len(ids))
		_, err := s.client.Pipelined(ctx, func(pipe redisclient.Pipeliner) error {
			for _, accountID := range ids[start:end] {
				id := strconv.FormatUint(accountID, 10)
				// EVAL avoids a NOSCRIPT fallback round trip inside the pipeline. Each
				// script remains bounded to one account so bulk maintenance cannot
				// monopolize the shared Redis event loop with one large Lua call.
				deleteStickyByAccountScript.Eval(ctx, pipe, []string{s.key("sticky-account", id)}, id)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ScheduleQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	if value.AccountID == 0 || value.Mode == "" || value.DueAt.IsZero() {
		return fmt.Errorf("额度恢复事件无效")
	}
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := scheduleQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), maxQuotaRecoveryEvents).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("额度恢复队列已满")
	}
	return nil
}

func (s *Store) MarkQuotaRefreshDirty(ctx context.Context, accountID uint64, mode string, ttl time.Duration) (repository.QuotaRefreshVersion, error) {
	mode = strings.TrimSpace(mode)
	if accountID == 0 || mode == "" || ttl <= 0 {
		return repository.QuotaRefreshVersion{}, fmt.Errorf("quota refresh identity is invalid")
	}
	member := strconv.FormatUint(accountID, 10) + ":" + mode
	now := time.Now().UTC()
	expiresAt := now.Add(ttl).Truncate(time.Millisecond)
	generation, err := markQuotaRefreshDirtyScript.Run(ctx, s.client,
		[]string{s.key("quota-refresh", "generations"), s.key("quota-refresh", "dirty"), s.key("quota-refresh", "expiry")},
		member, expiresAt.UnixMilli(), maxQuotaRefreshDirty, now.UnixMilli(),
	).Uint64()
	if err != nil {
		return repository.QuotaRefreshVersion{}, err
	}
	if generation == 0 {
		return repository.QuotaRefreshVersion{}, fmt.Errorf("quota refresh dirty set is full")
	}
	return repository.QuotaRefreshVersion{Generation: generation, ExpiresAt: expiresAt}, nil
}

func (s *Store) GetQuotaRefreshState(ctx context.Context, accountID uint64, mode string) (repository.QuotaRefreshVersion, bool, error) {
	member := strconv.FormatUint(accountID, 10) + ":" + strings.TrimSpace(mode)
	values, err := quotaRefreshStateScript.Run(ctx, s.client,
		[]string{s.key("quota-refresh", "generations"), s.key("quota-refresh", "dirty"), s.key("quota-refresh", "expiry")}, member, time.Now().UTC().UnixMilli(),
	).StringSlice()
	if err != nil {
		return repository.QuotaRefreshVersion{}, false, err
	}
	if len(values) != 3 {
		return repository.QuotaRefreshVersion{}, false, fmt.Errorf("quota refresh state response is invalid")
	}
	version, err := parseQuotaRefreshVersion(values[0], values[2])
	return version, values[1] == "1", err
}

func parseQuotaRefreshVersion(generationValue, expiryValue string) (repository.QuotaRefreshVersion, error) {
	generation, err := strconv.ParseUint(generationValue, 10, 64)
	if err != nil {
		return repository.QuotaRefreshVersion{}, err
	}
	expiry, err := strconv.ParseInt(expiryValue, 10, 64)
	if err != nil {
		return repository.QuotaRefreshVersion{}, err
	}
	if generation == 0 {
		return repository.QuotaRefreshVersion{}, nil
	}
	return repository.QuotaRefreshVersion{Generation: generation, ExpiresAt: time.UnixMilli(expiry).UTC()}, nil
}

func (s *Store) ClearQuotaRefreshDirty(ctx context.Context, accountID uint64, mode string, version repository.QuotaRefreshVersion) (bool, error) {
	member := strconv.FormatUint(accountID, 10) + ":" + strings.TrimSpace(mode)
	result, err := clearQuotaRefreshDirtyScript.Run(ctx, s.client,
		[]string{s.key("quota-refresh", "generations"), s.key("quota-refresh", "dirty"), s.key("quota-refresh", "expiry")},
		member, version.Generation, time.Now().UTC().UnixMilli(), version.ExpiresAt.UnixMilli(),
	).Int()
	return result == 1, err
}

func (s *Store) ScanQuotaRefreshDirty(ctx context.Context, now time.Time, cursor uint64, limit int) ([]repository.QuotaRefreshDirty, uint64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	values, err := scanQuotaRefreshDirtyScript.Run(ctx, s.client,
		[]string{s.key("quota-refresh", "generations"), s.key("quota-refresh", "dirty"), s.key("quota-refresh", "expiry")},
		now.UnixMilli(), limit, cursor,
	).StringSlice()
	if err != nil {
		return nil, cursor, err
	}
	if len(values) == 0 || (len(values)-1)%3 != 0 {
		return nil, cursor, fmt.Errorf("quota refresh page response is invalid")
	}
	next, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil {
		return nil, cursor, err
	}
	result := make([]repository.QuotaRefreshDirty, 0, min(limit, (len(values)-1)/3))
	for index := 1; index+2 < len(values); index += 3 {
		member := values[index]
		separator := strings.IndexByte(member, ':')
		if separator <= 0 || separator == len(member)-1 {
			continue
		}
		accountID, parseErr := strconv.ParseUint(member[:separator], 10, 64)
		version, versionErr := parseQuotaRefreshVersion(values[index+1], values[index+2])
		if parseErr != nil || versionErr != nil || accountID == 0 || version.Generation == 0 {
			continue
		}
		result = append(result, repository.QuotaRefreshDirty{AccountID: accountID, Mode: member[separator+1:], Version: version})
	}
	return result, next, nil
}

func (s *Store) EnsureQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	if value.AccountID == 0 || value.Mode == "" || value.DueAt.IsZero() {
		return fmt.Errorf("额度恢复事件无效")
	}
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := ensureQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), maxQuotaRecoveryEvents).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("额度恢复队列已满")
	}
	return nil
}

func (s *Store) CancelQuotaRecovery(ctx context.Context, accountID uint64, mode string) error {
	mode = strings.TrimSpace(mode)
	if accountID == 0 || mode == "" {
		return fmt.Errorf("额度恢复事件无效")
	}
	member := strconv.FormatUint(accountID, 10) + ":" + mode
	_, err := cancelQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member).Int()
	return err
}

func (s *Store) ClaimDueQuotaRecoveries(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]account.QuotaRecoveryEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	claimToken, err := randomToken()
	if err != nil {
		return nil, err
	}
	values, err := claimQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, now.UnixMilli(), limit, now.Add(lease).UnixMilli(), claimToken).StringSlice()
	if err != nil {
		return nil, err
	}
	result := make([]account.QuotaRecoveryEvent, 0, len(values)/3)
	for index := 0; index+2 < len(values); index += 3 {
		raw := values[index]
		idText, mode, ok := strings.Cut(raw, ":")
		id, parseErr := strconv.ParseUint(idText, 10, 64)
		attempts, attemptsErr := strconv.Atoi(values[index+1])
		if ok && parseErr == nil && id > 0 && mode != "" {
			if attemptsErr != nil || attempts < 0 {
				attempts = 0
			}
			result = append(result, account.QuotaRecoveryEvent{AccountID: id, Mode: mode, DueAt: now, Attempts: attempts, ClaimToken: values[index+2]})
		}
	}
	return result, nil
}

func (s *Store) AckQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := ackQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.ClaimToken).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) RescheduleQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := rescheduleQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), value.ClaimToken).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) Create(ctx context.Context, value account.DeviceSession) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ttl := time.Until(value.ExpiresAt)
	if ttl <= 0 {
		return repository.ErrNotFound
	}
	now := time.Now().UTC()
	result, err := createDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", value.ID), s.key("device-index", "sessions")}, payload, now.UnixMilli(), ttl.Milliseconds(), maxDeviceSessions, value.ExpiresAt.UnixMilli()).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) GetDevice(ctx context.Context, id string, now time.Time) (account.DeviceSession, error) {
	payload, err := s.client.Get(ctx, s.key("device", id)).Bytes()
	if errors.Is(err, redisclient.Nil) {
		return account.DeviceSession{}, repository.ErrNotFound
	}
	if err != nil {
		return account.DeviceSession{}, err
	}
	var value account.DeviceSession
	if err := json.Unmarshal(payload, &value); err != nil {
		return account.DeviceSession{}, err
	}
	if !now.Before(value.ExpiresAt) {
		if !now.Before(account.DeviceSessionRetentionUntil(value)) {
			_ = compareDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", id), s.key("device-index", "sessions")}, payload, "delete", "", 1, 0).Err()
		}
		return account.DeviceSession{}, repository.ErrNotFound
	}
	return value, nil
}

func (s *Store) mutateDeviceSession(ctx context.Context, id string, transition func(account.DeviceSession) (account.DeviceSession, bool, bool, error)) (account.DeviceSession, bool, error) {
	for attempt := 0; attempt < 16; attempt++ {
		payload, err := s.client.Get(ctx, s.key("device", id)).Bytes()
		if errors.Is(err, redisclient.Nil) {
			return account.DeviceSession{}, false, repository.ErrNotFound
		}
		if err != nil {
			return account.DeviceSession{}, false, err
		}
		var current account.DeviceSession
		if err := json.Unmarshal(payload, &current); err != nil {
			return account.DeviceSession{}, false, err
		}
		next, applied, remove, err := transition(current)
		if err != nil || !applied {
			return account.DeviceSession{}, false, err
		}
		operation, encoded, ttl := "delete", []byte(nil), int64(1)
		if !remove {
			if !time.Now().Before(account.DeviceSessionRetentionUntil(next)) {
				return account.DeviceSession{}, false, repository.ErrNotFound
			}
			ttl = max(int64(1), time.Until(account.DeviceSessionRetentionUntil(next)).Milliseconds())
			encoded, err = json.Marshal(next)
			if err != nil {
				return account.DeviceSession{}, false, err
			}
			operation = "update"
		}
		committed, err := compareDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", id), s.key("device-index", "sessions")}, payload, operation, encoded, ttl, account.DeviceSessionRetentionUntil(next).UnixMilli()).Int()
		if err != nil {
			return account.DeviceSession{}, false, err
		}
		if committed == 1 {
			return next, true, nil
		}
	}
	return account.DeviceSession{}, false, repository.ErrConflict
}

func (s *Store) acquireLock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}
	redisKey := s.key("lock", key)
	acquired, err := s.client.SetNX(ctx, redisKey, token, ttl).Result()
	if err != nil || !acquired {
		return nil, acquired, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = releaseLockScript.Run(releaseCtx, s.client, []string{redisKey}, token).Err()
		})
	}, true, nil
}

func randomToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

// DeviceSessionStore 适配 DeviceSessionRepository，避免与 StickySessionRepository 的 Get 签名冲突。
type DeviceSessionStore struct{ store *Store }

func NewDeviceSessionStore(store *Store) *DeviceSessionStore {
	return &DeviceSessionStore{store: store}
}
func (s *DeviceSessionStore) Create(ctx context.Context, value account.DeviceSession) error {
	return s.store.Create(ctx, value)
}
func (s *DeviceSessionStore) Get(ctx context.Context, id string, now time.Time) (account.DeviceSession, error) {
	return s.store.GetDevice(ctx, id, now)
}
func (s *DeviceSessionStore) ClaimPoll(ctx context.Context, id, token string, now, leaseUntil time.Time) (account.DeviceSession, error) {
	next, _, err := s.store.mutateDeviceSession(ctx, id, func(current account.DeviceSession) (account.DeviceSession, bool, bool, error) {
		next, err := account.ClaimDevicePoll(current, token, now, leaseUntil)
		return next, err == nil, false, err
	})
	if errors.Is(err, account.ErrDeviceSessionExpired) {
		err = repository.ErrNotFound
	}
	return next, err
}
func (s *DeviceSessionStore) FinishPoll(ctx context.Context, receipt account.DevicePollReceipt, event account.DevicePollCompletion) (bool, error) {
	_, applied, err := s.store.mutateDeviceSession(ctx, receipt.SessionID, func(current account.DeviceSession) (account.DeviceSession, bool, bool, error) {
		return account.CompleteDevicePoll(current, receipt, event)
	})
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	return applied, err
}

// ConcurrencyLimiter 适配 ConcurrencyLimiter，避免与 DistributedLock 的 Acquire 签名冲突。
type ConcurrencyLimiter struct{ store *Store }

func NewConcurrencyLimiter(store *Store) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{store: store}
}
func (l *ConcurrencyLimiter) Acquire(ctx context.Context, key string, limit int) (func(), bool, error) {
	return l.store.acquireConcurrency(ctx, key, limit)
}
func (l *ConcurrencyLimiter) AcquireBounded(ctx context.Context, key string, limit int, ttl time.Duration) (func(), bool, error) {
	return l.store.acquireConcurrencyFor(ctx, key, limit, ttl)
}
func (l *ConcurrencyLimiter) Current(ctx context.Context, key string) (int, error) {
	return l.store.Current(ctx, key)
}
func (l *ConcurrencyLimiter) CurrentMany(ctx context.Context, keys []string) (map[string]int, error) {
	return l.store.CurrentMany(ctx, keys)
}

// LockStore 适配 DistributedLock。
type LockStore struct{ store *Store }

func NewLockStore(store *Store) *LockStore { return &LockStore{store: store} }
func (l *LockStore) Acquire(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	return l.store.acquireLock(ctx, strings.TrimSpace(key), ttl)
}
