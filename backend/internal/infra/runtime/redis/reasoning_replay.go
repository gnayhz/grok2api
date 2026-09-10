package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	redisclient "github.com/redis/go-redis/v9"
)

// ReasoningReplayStore 基于 Redis 的推理回放仓储，支持多实例共享。
type ReasoningReplayStore struct {
	store *Store
}

// NewReasoningReplayStore 包装已打开的运行态 Store。
func NewReasoningReplayStore(store *Store) *ReasoningReplayStore {
	return &ReasoningReplayStore{store: store}
}

func (s *ReasoningReplayStore) redisKey(model, sessionKey string) string {
	return s.store.key("reasoning-replay", hashKeyPart(model)+":"+hashKeyPart(sessionKey))
}

func hashKeyPart(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:16])
}

func (s *ReasoningReplayStore) Get(ctx context.Context, model, sessionKey string, now time.Time, ttl time.Duration) ([][]byte, bool, error) {
	if s == nil || s.store == nil || model == "" || sessionKey == "" {
		return nil, false, nil
	}
	_ = now
	key := s.redisKey(model, sessionKey)
	raw, err := s.store.client.Get(ctx, key).Bytes()
	if errors.Is(err, redisclient.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var items [][]byte
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false, err
	}
	// 滑动续期：命中后恢复为完整配置 TTL。
	if ttl > 0 {
		_ = s.store.client.Expire(ctx, key, ttl).Err()
	}
	return cloneItems(items), true, nil
}

func (s *ReasoningReplayStore) Set(ctx context.Context, model, sessionKey string, items [][]byte, expiresAt time.Time) error {
	if s == nil || s.store == nil || model == "" || sessionKey == "" || len(items) == 0 {
		return nil
	}
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return s.store.client.Set(ctx, s.redisKey(model, sessionKey), raw, ttl).Err()
}

func (s *ReasoningReplayStore) Delete(ctx context.Context, model, sessionKey string) error {
	if s == nil || s.store == nil || model == "" || sessionKey == "" {
		return nil
	}
	return s.store.client.Del(ctx, s.redisKey(model, sessionKey)).Err()
}

func cloneItems(items [][]byte) [][]byte {
	if len(items) == 0 {
		return nil
	}
	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, append([]byte(nil), item...))
	}
	return cloned
}

// WATCH protects the merge across gateway instances, including concurrent
// deletion/expiry. Read/decode failures must never overwrite existing history.
func (s *ReasoningReplayStore) Update(ctx context.Context, model, sessionKey string, expiresAt time.Time, merge func([][]byte) [][]byte) error {
	if s == nil || s.store == nil || model == "" || sessionKey == "" || merge == nil {
		return nil
	}
	key := s.redisKey(model, sessionKey)
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.store.client.Watch(ctx, func(tx *redisclient.Tx) error {
			raw, err := tx.Get(ctx, key).Bytes()
			var existing [][]byte
			if err != nil && !errors.Is(err, redisclient.Nil) {
				return err
			}
			if err == nil {
				if err := json.Unmarshal(raw, &existing); err != nil {
					return err
				}
			}
			next := merge(existing)
			ttl := time.Until(expiresAt)
			if len(next) == 0 || ttl <= 0 {
				return nil
			}
			encoded, err := json.Marshal(next)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redisclient.Pipeliner) error { pipe.Set(ctx, key, encoded, ttl); return nil })
			return err
		}, key)
		if !errors.Is(err, redisclient.TxFailedErr) {
			return err
		}
		perfmetrics.Default.Inc("reasoning_replay_update_conflicts_total", perfmetrics.Labels{Subsystem: "reasoning_replay", Plane: "redis", Outcome: "retry"})
	}
	return redisclient.TxFailedErr
}
