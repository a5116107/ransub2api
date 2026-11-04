package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const upstreamKeyPolicyKey = "concurrency:upstream_key:policy"
const upstreamKeyLeaseTTL = 30 * time.Second
const upstreamKeyLeaseValidFor = 19 * time.Second
const upstreamKeyRenewInterval = 5 * time.Second

// One authoritative policy is used across accounts/products/processes. Older
// revisions and conflicting equal revisions fail closed. Policy changes retain
// the same slot keys, so shrinking or removing a limit never resets occupancy.
var acquireUpstreamKeyScript = redis.NewScript(`
redis.replicate_commands()
local revision = tonumber(ARGV[1])
local current = tonumber(redis.call('HGET', KEYS[1], 'revision') or '0')
if current > revision then return -1 end
if current == revision and redis.call('HGET', KEYS[1], 'digest') ~= ARGV[2] then return -1 end
local limits = cjson.decode(ARGV[3])
if current < revision then
  redis.call('HSET', KEYS[1], 'revision', ARGV[1], 'digest', ARGV[2])
end
local limit = limits[ARGV[4]]
if not limit then return 2 end
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local ttl = tonumber(ARGV[6])
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now - ttl)
if redis.call('ZSCORE', KEYS[2], ARGV[5]) then return 1 end
if redis.call('ZCARD', KEYS[2]) >= limit then return 0 end
redis.call('ZADD', KEYS[2], now, ARGV[5])
redis.call('PEXPIRE', KEYS[2], ttl)
return 1
`)

var refreshUpstreamKeyScript = redis.NewScript(`
redis.replicate_commands()
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - tonumber(ARGV[2]))
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) then return 0 end
redis.call('ZADD', KEYS[1], now, ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`)

type upstreamKeyLimiter struct {
	rdb     *redis.Client
	policy  config.UpstreamKeyConcurrencyConfig
	encoded string
	digest  string
	err     error
}

func newUpstreamKeyLimiter(cfg *config.Config, rdb *redis.Client) *upstreamKeyLimiter {
	if cfg == nil || !cfg.Gateway.UpstreamKeyConcurrency.Configured() {
		return nil
	}
	l := &upstreamKeyLimiter{rdb: rdb, policy: cfg.Gateway.UpstreamKeyConcurrency}
	l.err = l.policy.Validate()
	if cfg.Runtime.RegionalConcurrencyFile != "" || cfg.Runtime.RegionalSchedulerFile != "" || rdb == nil {
		l.err = service.ErrUpstreamKeyPolicy
	}
	limits := l.policy.Limits
	if limits == nil {
		limits = map[string]int{}
	}
	encoded, err := json.Marshal(limits)
	if err != nil {
		l.err = err
	}
	l.encoded = string(encoded)
	digest := sha256.Sum256(encoded)
	l.digest = hex.EncodeToString(digest[:])
	return l
}

type upstreamKeyLease struct {
	limiter  *upstreamKeyLimiter
	key      string
	member   string
	cancel   context.CancelCauseFunc
	stop     context.CancelFunc
	done     chan struct{}
	watchdog *time.Timer
	mu       sync.Mutex
	stopped  bool
	once     sync.Once
}

func (l *upstreamKeyLimiter) acquire(ctx context.Context, fingerprint string, cancel context.CancelCauseFunc) (*upstreamKeyLease, error) {
	if l.err != nil {
		return nil, service.ErrUpstreamKeyPolicy
	}
	key := "concurrency:upstream_key:slots:" + fingerprint
	member := uuid.NewString()
	started := time.Now()
	opCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	result, err := acquireUpstreamKeyScript.Run(opCtx, l.rdb, []string{upstreamKeyPolicyKey, key}, l.policy.Revision, l.digest, l.encoded, fingerprint, member, upstreamKeyLeaseTTL.Milliseconds()).Int()
	stop()
	if err != nil {
		l.release(key, member)
		return nil, service.ErrUpstreamKeyPolicy
	}
	if result == -1 {
		return nil, service.ErrUpstreamKeyPolicy
	}
	if result == 0 {
		return nil, service.ErrUpstreamKeyCapacity
	}
	if result == 2 {
		return nil, nil
	}
	if result != 1 {
		l.release(key, member)
		return nil, service.ErrUpstreamKeyPolicy
	}
	workerCtx, workerStop := context.WithCancel(context.Background())
	lease := &upstreamKeyLease{limiter: l, key: key, member: member, cancel: cancel, stop: workerStop, done: make(chan struct{})}
	lease.watchdog = time.AfterFunc(time.Until(started.Add(upstreamKeyLeaseValidFor)), lease.lose)
	go lease.renew(workerCtx)
	if ctx.Err() != nil || time.Since(started) >= upstreamKeyLeaseValidFor {
		lease.release()
		return nil, service.ErrUpstreamKeyLease
	}
	return lease, nil
}

func (l *upstreamKeyLease) lose() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.stopped {
		l.stopped = true
		l.cancel(service.ErrUpstreamKeyLease)
		l.stop()
	}
}

func (l *upstreamKeyLease) renew(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(upstreamKeyRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			started := time.Now()
			opCtx, stop := context.WithTimeout(ctx, time.Second)
			exists, err := refreshUpstreamKeyScript.Run(opCtx, l.limiter.rdb, []string{l.key}, l.member, upstreamKeyLeaseTTL.Milliseconds()).Int()
			stop()
			if err == nil && exists == 0 {
				l.lose()
				return
			}
			if err == nil {
				l.mu.Lock()
				if !l.stopped {
					l.watchdog.Reset(time.Until(started.Add(upstreamKeyLeaseValidFor)))
				}
				l.mu.Unlock()
			}
		}
	}
}

func (l *upstreamKeyLimiter) release(key, member string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.rdb.ZRem(ctx, key, member).Err(); err != nil {
		slog.Warn("upstream_key_slot_release_failed")
	}
}

func (l *upstreamKeyLease) release() {
	l.once.Do(func() {
		l.mu.Lock()
		l.stopped = true
		l.watchdog.Stop()
		l.mu.Unlock()
		l.stop()
		<-l.done
		l.limiter.release(l.key, l.member)
	})
}

// AcquireUpstreamKeySlots shares one authority across HTTP response lifetimes
// and native WS turns. A nil release means no configured credential held a slot.
func (s *httpUpstreamService) AcquireUpstreamKeySlots(parent context.Context, fingerprints []string) (context.Context, func(), error) {
	if s.upstreamKeyLimiter == nil {
		return parent, nil, nil
	}
	if s.upstreamKeyLimiter.err != nil {
		return parent, nil, service.ErrUpstreamKeyPolicy
	}
	ctx, cancel := context.WithCancelCause(parent)
	var leases []*upstreamKeyLease
	release := sync.OnceFunc(func() {
		cancel(context.Canceled)
		for _, lease := range leases {
			lease.release()
		}
	})
	seen := make(map[string]struct{}, len(fingerprints))
	for _, fingerprint := range fingerprints {
		if _, exists := seen[fingerprint]; exists {
			continue
		}
		seen[fingerprint] = struct{}{}
		lease, err := s.upstreamKeyLimiter.acquire(ctx, fingerprint, cancel)
		if err != nil {
			release()
			return parent, nil, err
		}
		if lease != nil {
			leases = append(leases, lease)
		}
	}
	if len(leases) == 0 {
		release()
		return parent, nil, nil
	}
	return ctx, release, nil
}

// beginUpstreamKeyRequest retains response-body ownership at the existing
// HTTP boundary; native WS owns its active turn through the shared method above.
func (s *httpUpstreamService) beginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	if s.upstreamKeyLimiter == nil {
		return req, func(*http.Response, error) {}, nil
	}
	if req == nil || req.URL == nil {
		return req, nil, service.ErrUpstreamKeyPolicy
	}
	ctx, release, err := s.AcquireUpstreamKeySlots(req.Context(), config.UpstreamCredentialFingerprints(req))
	if err != nil {
		return req, nil, err
	}
	if release == nil {
		return req, func(*http.Response, error) {}, nil
	}
	// SDP completes before a realtime session; its lease needs a separate handoff.
	if strings.Contains(strings.ToLower(req.URL.Path), "/realtime") {
		release()
		return req, nil, service.ErrUpstreamKeyPolicy
	}
	return req.WithContext(ctx), func(resp *http.Response, err error) {
		if err != nil || resp == nil || resp.Body == nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			release()
			return
		}
		body := &upstreamKeyResponseBody{ReadCloser: resp.Body, release: release}
		resp.Body = body
		context.AfterFunc(ctx, func() { _ = body.Close() })
	}, nil
}

type upstreamKeyResponseBody struct {
	io.ReadCloser
	release  func()
	once     sync.Once
	closeErr error
}

func (b *upstreamKeyResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		_ = b.Close()
	}
	return n, err
}

func (b *upstreamKeyResponseBody) Close() error {
	b.once.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.release()
	})
	return b.closeErr
}
