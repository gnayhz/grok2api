package redis

import (
	"context"
	"errors"
	redisclient "github.com/redis/go-redis/v9"
	"time"
)

var rollingRateScript = redisclient.NewScript(`
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local window = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - window)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[1]) then
 local oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
 return {0, tonumber(oldest[2]) + window - now}
end
redis.call('ZADD', KEYS[1], now, ARGV[3])
redis.call('PEXPIRE', KEYS[1], window)
return {1, 0}
`)

func (s *Store) AllowRolling(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if limit <= 0 || window < time.Millisecond {
		return false, 0, errors.New("invalid rolling rate limit")
	}
	token, err := randomToken()
	if err != nil {
		return false, 0, err
	}
	result, err := rollingRateScript.Run(ctx, s.client, []string{s.key("rolling_rate", key)}, limit, window.Milliseconds(), token).Int64Slice()
	if err != nil {
		return false, 0, err
	}
	if len(result) != 2 {
		return false, 0, errors.New("invalid rolling rate result")
	}
	return result[0] == 1, time.Duration(result[1]) * time.Millisecond, nil
}
