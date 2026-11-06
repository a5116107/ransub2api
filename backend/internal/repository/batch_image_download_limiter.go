package repository

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	defaultBatchImageDownloadActivePrefix = "batch_image:download:active:"
	defaultBatchImageDownloadActiveTTL    = 10 * time.Minute
	defaultBatchImageDownloadConcurrency  = 2
)

var batchImageDownloadAcquireScript = redis.NewScript(`
redis.replicate_commands()
local kind = redis.call('TYPE', KEYS[1]).ok
if kind ~= 'none' and kind ~= 'zset' then return -1 end
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[1]) then
  return 0
end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[2]), ARGV[3])
local latest = redis.call('ZREVRANGE', KEYS[1], 0, 0, 'WITHSCORES')
redis.call('PEXPIREAT', KEYS[1], latest[2])
return 1
`)

var batchImageDownloadReleaseScript = redis.NewScript(`
local kind = redis.call('TYPE', KEYS[1]).ok
if kind ~= 'zset' then return 0 end
redis.call('ZREM', KEYS[1], ARGV[1])
if redis.call('ZCARD', KEYS[1]) == 0 then redis.call('DEL', KEYS[1]) end
return 1
`)

type batchImageDownloadLimiter struct {
	rdb          *redis.Client
	activePrefix string
	maxActive    int
	ttl          time.Duration
}

func NewBatchImageDownloadLimiter(rdb *redis.Client, cfg *config.Config) service.BatchImageDownloadLimiter {
	maxActive := defaultBatchImageDownloadConcurrency
	ttl := defaultBatchImageDownloadActiveTTL
	if cfg != nil {
		if cfg.BatchImage.MaxDownloadConcurrencyPerUser > 0 {
			maxActive = cfg.BatchImage.MaxDownloadConcurrencyPerUser
		}
		if cfg.BatchImage.MaxDownloadDurationSeconds > 0 {
			ttl = time.Duration(cfg.BatchImage.MaxDownloadDurationSeconds) * time.Second
		}
	}
	return &batchImageDownloadLimiter{
		rdb:          rdb,
		activePrefix: defaultBatchImageDownloadActivePrefix,
		maxActive:    maxActive,
		ttl:          ttl,
	}
}

func (l *batchImageDownloadLimiter) Acquire(ctx context.Context, userID string, kind string) (service.BatchImageDownloadPermit, error) {
	if l == nil || l.rdb == nil || l.maxActive <= 0 || l.ttl < time.Millisecond {
		return nil, service.ErrBatchImageDownloadLimited
	}
	key := l.activeKey(userID)
	permit := &batchImageDownloadPermit{rdb: l.rdb, key: key, member: uuid.NewString()}
	ok, err := batchImageDownloadAcquireScript.Run(ctx, l.rdb, []string{key}, l.maxActive, l.ttl.Milliseconds(), permit.member).Int()
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = permit.Release(cleanupCtx)
		return nil, err
	}
	if ok != 1 {
		return nil, service.ErrBatchImageDownloadLimited
	}
	return permit, nil
}

func (l *batchImageDownloadLimiter) activeKey(userID string) string {
	return l.activePrefix + userID
}

type batchImageDownloadPermit struct {
	rdb      *redis.Client
	key      string
	member   string
	mu       sync.Mutex
	released bool
}

func (p *batchImageDownloadPermit) Release(ctx context.Context) error {
	if p == nil || p.rdb == nil || p.key == "" || p.member == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return nil
	}
	_, err := batchImageDownloadReleaseScript.Run(ctx, p.rdb, []string{p.key}, p.member).Result()
	if err == nil {
		p.released = true
	}
	return err
}

var _ service.BatchImageDownloadLimiter = (*batchImageDownloadLimiter)(nil)
var _ service.BatchImageDownloadPermit = (*batchImageDownloadPermit)(nil)
