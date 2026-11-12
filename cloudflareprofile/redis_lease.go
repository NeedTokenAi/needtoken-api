package cloudflareprofile

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	writerLeaseKey = "needtoken-api:cloudflare:writer-lease"
	writerEpochKey = "needtoken-api:cloudflare:writer-epoch"
)

// acquireScript returns the lease value "epoch|holder" or an empty string
// when another holder owns an unexpired lease. The epoch key is not deleted
// when the lease expires, so the next holder gets a greater epoch.
const acquireScript = `
local lease = redis.call('GET', KEYS[1])
local holder = ARGV[1]
local ttl = ARGV[2]
if lease then
  local sep = string.find(lease, '|', 1, true)
  local current = sep and string.sub(lease, sep + 1) or ''
  if current == holder then
    redis.call('PEXPIRE', KEYS[1], ttl)
    return lease
  end
  return ''
end
local epoch = redis.call('INCR', KEYS[2])
local value = tostring(epoch) .. '|' .. holder
local ok = redis.call('SET', KEYS[1], value, 'PX', ttl, 'NX')
if not ok then
  return ''
end
return value
`

const renewScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
`

const releaseScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`

type redisLease struct {
	client *redis.Client
}

func newRedisLease(client *redis.Client) *redisLease {
	return &redisLease{client: client}
}

func (l *redisLease) Acquire(ctx context.Context, holder string, ttl time.Duration) (int64, error) {
	if l == nil || l.client == nil {
		return 0, errors.New(errRedisClient)
	}
	reply, err := l.client.Eval(ctx, acquireScript, []string{writerLeaseKey, writerEpochKey}, holder, ttl.Milliseconds()).Result()
	if err != nil {
		return 0, err
	}
	value, _ := reply.(string)
	epoch, owner, ok := parseLease(value)
	if !ok || owner != holder {
		return 0, ErrWriterHeld
	}
	return epoch, nil
}

func (l *redisLease) Renew(ctx context.Context, holder string, epoch int64, ttl time.Duration) error {
	if l == nil || l.client == nil {
		return errors.New(errRedisClient)
	}
	reply, err := l.client.Eval(ctx, renewScript, []string{writerLeaseKey}, formatLease(epoch, holder), ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if reply != 1 {
		return ErrWriterLost
	}
	return nil
}

func (l *redisLease) Release(ctx context.Context, holder string, epoch int64) error {
	if l == nil || l.client == nil {
		return errors.New(errRedisClient)
	}
	_, err := l.client.Eval(ctx, releaseScript, []string{writerLeaseKey}, formatLease(epoch, holder)).Result()
	return err
}

func formatLease(epoch int64, holder string) string {
	return strconv.FormatInt(epoch, 10) + "|" + holder
}

func parseLease(value string) (int64, string, bool) {
	epochText, holder, ok := strings.Cut(value, "|")
	if !ok || holder == "" || strings.Contains(holder, "|") {
		return 0, "", false
	}
	epoch, err := strconv.ParseInt(epochText, 10, 64)
	if err != nil || epoch <= 0 {
		return 0, "", false
	}
	return epoch, holder, true
}
