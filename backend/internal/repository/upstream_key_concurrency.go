package repository

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const upstreamKeyPolicyKey = "concurrency:upstream_key:policy"
const upstreamKeySlotKeyPrefix = "concurrency:upstream_key:slots:"
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
if ARGV[7] == 'persistent' then
  redis.call('ZADD', KEYS[2], '+inf', ARGV[5])
else
  redis.call('ZADD', KEYS[2], now, ARGV[5])
end
if redis.call('ZCOUNT', KEYS[2], '+inf', '+inf') > 0 then
  redis.call('PERSIST', KEYS[2])
else
  redis.call('PEXPIRE', KEYS[2], ttl)
end
return 1
`)

var refreshUpstreamKeyScript = redis.NewScript(`
redis.replicate_commands()
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - tonumber(ARGV[2]))
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) then return 0 end
redis.call('ZADD', KEYS[1], now, ARGV[1])
if redis.call('ZCOUNT', KEYS[1], '+inf', '+inf') > 0 then
  redis.call('PERSIST', KEYS[1])
else
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 1
`)

type upstreamKeyLimiter struct {
	rdb      *redis.Client
	regional *regionalConcurrencyCache
	policy   config.UpstreamKeyConcurrencyConfig
	encoded  string
	digest   string
	err      error
}

func newUpstreamKeyLimiter(cfg *config.Config, rdb *redis.Client, cache service.ConcurrencyCache) *upstreamKeyLimiter {
	regional, _ := cache.(*regionalConcurrencyCache)
	if cfg == nil || (!cfg.Gateway.UpstreamKeyConcurrency.Configured() && regional == nil && cfg.Runtime.RegionalConcurrencyFile == "" && cfg.Runtime.RegionalSchedulerFile == "") {
		return nil
	}
	l := &upstreamKeyLimiter{rdb: rdb, regional: regional, policy: cfg.Gateway.UpstreamKeyConcurrency}
	l.err = l.policy.Validate()
	if regional == nil && (cfg.Runtime.RegionalConcurrencyFile != "" || cfg.Runtime.RegionalSchedulerFile != "" || rdb == nil) {
		l.err = service.ErrUpstreamKeyPolicy
	}
	encoded, digest, err := l.policy.EncodeLimits()
	if err != nil {
		l.err = err
	}
	l.encoded = string(encoded)
	l.digest = digest
	return l
}

type upstreamKeyLease struct {
	limiter  *upstreamKeyLimiter
	key      string
	resource slots.Resource
	member   string
	cancel   context.CancelCauseFunc
	stop     context.CancelFunc
	done     chan struct{}
	watchdog *time.Timer
	mu       sync.Mutex
	stopped  bool
	once     sync.Once
}

func (l *upstreamKeyLimiter) take(ctx context.Context, fingerprint, member, mode string) (slots.Resource, int, error) {
	if l.err != nil {
		return slots.Resource{}, 0, service.ErrUpstreamKeyPolicy
	}
	key := upstreamKeySlotKeyPrefix + fingerprint
	opCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	var result int
	var resource slots.Resource
	var err error
	if l.regional != nil {
		resource, result, err = l.regional.acquireUpstreamKeySlot(opCtx, l.policy.Revision, l.digest, fingerprint, member)
	} else {
		result, err = acquireUpstreamKeyScript.Run(opCtx, l.rdb, []string{upstreamKeyPolicyKey, key}, l.policy.Revision, l.digest, l.encoded, fingerprint, member, upstreamKeyLeaseTTL.Milliseconds(), mode).Int()
	}
	if err != nil {
		_ = l.release(resource, key, member)
		return resource, 0, service.ErrUpstreamKeyPolicy
	}
	if result == -1 {
		return resource, result, service.ErrUpstreamKeyPolicy
	}
	if result == 0 {
		return resource, result, service.ErrUpstreamKeyCapacity
	}
	if result != 1 && result != 2 {
		_ = l.release(resource, key, member)
		return resource, result, service.ErrUpstreamKeyPolicy
	}
	return resource, result, nil
}

func (l *upstreamKeyLimiter) acquire(ctx context.Context, fingerprint string, cancel context.CancelCauseFunc) (*upstreamKeyLease, error) {
	key, member := upstreamKeySlotKeyPrefix+fingerprint, uuid.NewString()
	started := time.Now()
	resource, result, err := l.take(ctx, fingerprint, member, "")
	if err != nil || result == 2 {
		return nil, err
	}
	workerCtx, workerStop := context.WithCancel(context.Background())
	lease := &upstreamKeyLease{limiter: l, key: key, resource: resource, member: member, cancel: cancel, stop: workerStop, done: make(chan struct{})}
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
			var exists int
			var err error
			if l.limiter.regional != nil {
				var held bool
				held, err = l.limiter.regional.upstreamKeyHeld(opCtx, l.resource, l.member)
				if held {
					exists = 1
				}
			} else {
				exists, err = refreshUpstreamKeyScript.Run(opCtx, l.limiter.rdb, []string{l.key}, l.member, upstreamKeyLeaseTTL.Milliseconds()).Int()
			}
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

func (l *upstreamKeyLimiter) release(resource slots.Resource, key, member string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if l.regional != nil {
		if resource.Kind == "" {
			return nil
		}
		err = l.regional.releaseUpstreamKeySlot(ctx, resource, member)
	} else {
		err = l.rdb.ZRem(ctx, key, member).Err()
	}
	if err != nil {
		slog.Warn("upstream_key_slot_release_failed")
	}
	return err
}

func (l *upstreamKeyLease) release() {
	l.once.Do(func() {
		l.mu.Lock()
		l.stopped = true
		l.watchdog.Stop()
		l.mu.Unlock()
		l.stop()
		<-l.done
		_ = l.limiter.release(l.resource, l.key, l.member)
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
	for _, fingerprint := range fingerprints {
		if fingerprint == config.UnverifiedUpstreamAgentCredential {
			return parent, nil, service.ErrUpstreamKeyPolicy
		}
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

func (l *upstreamKeyLimiter) persistentAuthority() string {
	if l.regional != nil {
		if l.regional.primary != nil {
			return "regional:primary"
		}
		return "regional:local:" + l.regional.region()
	}
	return "redis"
}

func samePersistentCredentials(expected, actual []string) bool {
	if len(expected) != len(actual) {
		return false
	}
	seen := make(map[string]bool, len(expected))
	for _, fingerprint := range expected {
		seen[fingerprint] = true
	}
	for _, fingerprint := range actual {
		if !seen[fingerprint] {
			return false
		}
		delete(seen, fingerprint)
	}
	return len(seen) == 0
}

func (s *httpUpstreamService) AcquirePersistentUpstreamKeySlots(ctx context.Context, req *http.Request) (*service.PersistentUpstreamKeyLease, error) {
	l := s.upstreamKeyLimiter
	if l == nil {
		if service.BatchImageKeyPolicyRequired(req) {
			return nil, service.ErrUpstreamKeyPolicy
		}
		return nil, nil
	}
	if req == nil || req.URL == nil || l.err != nil || !service.BatchImageKeyPolicyMatches(req, l.policy.Revision, l.digest) {
		return nil, service.ErrUpstreamKeyPolicy
	}
	member := service.BatchImageKeyCreationMember(req)
	if member == "" {
		if !strings.HasSuffix(req.URL.Path, "/realtime/calls") {
			return nil, service.ErrUpstreamKeyPolicy
		}
		member = "live:" + uuid.NewString()
	}
	fingerprints := config.UpstreamCredentialFingerprints(req)
	for _, fingerprint := range fingerprints {
		if fingerprint == config.UnverifiedUpstreamAgentCredential {
			return nil, service.ErrUpstreamKeyPolicy
		}
	}
	lease := &service.PersistentUpstreamKeyLease{Authority: l.persistentAuthority(), Member: member, Fingerprints: fingerprints}
	if l.regional == nil {
		for _, fingerprint := range lease.Fingerprints {
			if l.policy.Limits[fingerprint] > 0 {
				if err := l.redisPersistentDurable(ctx); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	for _, fingerprint := range lease.Fingerprints {
		resource, result, err := l.take(ctx, fingerprint, lease.Member, "persistent")
		if err != nil {
			// Creation was not sent. Only this attempt's members may go back.
			for _, held := range lease.Resources {
				_ = l.release(slots.Resource{Kind: "upstream_key", ID: held.ResourceID, Fingerprint: held.Fingerprint}, upstreamKeySlotKeyPrefix+held.Fingerprint, lease.Member)
			}
			return nil, err
		}
		if result == 1 {
			lease.Resources = append(lease.Resources, service.PersistentUpstreamKeyResource{Fingerprint: fingerprint, ResourceID: resource.ID})
		}
	}
	if len(lease.Resources) == 0 {
		return nil, nil
	}
	return lease, nil
}

func (l *upstreamKeyLimiter) redisPersistentDurable(ctx context.Context) error {
	// Remote work cannot be cancelled by an expiring local watchdog. Refuse
	// admission when Redis may acknowledge volatile/evictable ownership.
	for field, expected := range map[string]string{"appendonly": "yes", "appendfsync": "always", "no-appendfsync-on-rewrite": "no", "maxmemory-policy": "noeviction"} {
		values, err := l.rdb.ConfigGet(ctx, field).Result()
		if err != nil || values[field] != expected {
			return service.ErrUpstreamKeyPolicy
		}
	}
	return nil
}

func (s *httpUpstreamService) CheckPersistentUpstreamKeySlots(ctx context.Context, lease *service.PersistentUpstreamKeyLease) error {
	l := s.upstreamKeyLimiter
	if l == nil || l.err != nil || lease.Validate() != nil || lease.Authority != l.persistentAuthority() {
		return service.ErrUpstreamKeyPolicy
	}
	for _, resource := range lease.Resources {
		var held bool
		var err error
		if l.regional != nil {
			held, err = l.regional.upstreamKeyHeld(ctx, slots.Resource{Kind: "upstream_key", ID: resource.ResourceID, Fingerprint: resource.Fingerprint}, lease.Member)
		} else {
			var score float64
			score, err = l.rdb.ZScore(ctx, upstreamKeySlotKeyPrefix+resource.Fingerprint, lease.Member).Result()
			held = err == nil && math.IsInf(score, 1)
		}
		if err != nil || !held {
			return service.ErrUpstreamKeyLease
		}
	}
	return nil
}

func (l *upstreamKeyLimiter) confirmPersistentPolicy(ctx context.Context, lease *service.PersistentUpstreamKeyLease) error {
	limited := make(map[string]bool, len(lease.Resources))
	for _, fingerprint := range lease.Fingerprints {
		if l.policy.Limits[fingerprint] > 0 {
			limited[fingerprint] = true
		}
	}
	if len(limited) != len(lease.Resources) {
		return service.ErrUpstreamKeyPolicy
	}
	for _, resource := range lease.Resources {
		if !limited[resource.Fingerprint] {
			return service.ErrUpstreamKeyPolicy
		}
	}
	if l.regional != nil {
		return l.regional.confirmPersistentUpstreamPolicy(ctx, l.policy.Revision, l.digest, lease.Resources)
	}
	// HMGET is one atomic read of the same policy published by acquisition.
	values, err := l.rdb.HMGet(ctx, upstreamKeyPolicyKey, "revision", "digest").Result()
	if err != nil || len(values) != 2 || values[0] != strconv.FormatInt(l.policy.Revision, 10) || values[1] != l.digest {
		return service.ErrUpstreamKeyPolicy
	}
	return l.redisPersistentDurable(ctx)
}

func (s *httpUpstreamService) ReleasePersistentUpstreamKeySlots(_ context.Context, lease *service.PersistentUpstreamKeyLease) error {
	l := s.upstreamKeyLimiter
	if l == nil || lease.Validate() != nil || lease.Authority != l.persistentAuthority() {
		return service.ErrUpstreamKeyPolicy
	}
	var releaseErr error
	for _, resource := range lease.Resources {
		err := l.release(slots.Resource{Kind: "upstream_key", ID: resource.ResourceID, Fingerprint: resource.Fingerprint}, upstreamKeySlotKeyPrefix+resource.Fingerprint, lease.Member)
		releaseErr = errors.Join(releaseErr, err)
	}
	return releaseErr
}

func (*httpUpstreamService) SupportsAsyncVideoKeyGuard() bool { return true }

// BeginUpstreamKeyRequest retains response-body ownership at the existing
// HTTP boundary; native WS owns its active turn through the shared method above.
func (s *httpUpstreamService) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	asyncVideo, asyncRequired := service.AsyncVideoKeyGuardState(req)
	if s.upstreamKeyLimiter == nil {
		if service.BatchImageKeyPolicyRequired(req) || asyncRequired {
			return req, nil, service.ErrUpstreamKeyPolicy
		}
		return req, func(*http.Response, error) {}, nil
	}
	if req == nil || req.URL == nil {
		return req, nil, service.ErrUpstreamKeyPolicy
	}
	if !service.BatchImageKeyPolicyMatches(req, s.upstreamKeyLimiter.policy.Revision, s.upstreamKeyLimiter.digest) {
		return req, nil, service.ErrUpstreamKeyPolicy
	}
	if lease := service.PersistentUpstreamKeyLeaseFromContext(req.Context()); lease != nil {
		if asyncVideo {
			return req, nil, service.ErrUpstreamKeyPolicy
		}
		batch := strings.HasPrefix(lease.Member, "batch:")
		if (batch && lease.Member != service.BatchImageKeyRequestMember(req)) ||
			(!batch && !strings.HasSuffix(req.URL.Path, "/realtime/calls")) ||
			!samePersistentCredentials(lease.Fingerprints, config.UpstreamCredentialFingerprints(req)) {
			return req, nil, service.ErrUpstreamKeyPolicy
		}
		if err := s.CheckPersistentUpstreamKeySlots(req.Context(), lease); err != nil {
			return req, nil, errors.Join(service.ErrUpstreamKeyPolicy, err)
		}
		if !batch || service.BatchImageKeyCreationMember(req) != "" {
			if err := s.upstreamKeyLimiter.confirmPersistentPolicy(req.Context(), lease); err != nil {
				return req, nil, errors.Join(service.ErrUpstreamKeyPolicy, err)
			}
		}
		// The controller owns this member, independently of body close/cancel.
		return req, func(*http.Response, error) {}, nil
	}
	fingerprints := config.UpstreamCredentialFingerprints(req)
	if asyncVideo && len(fingerprints) == 0 {
		return req, nil, service.ErrUpstreamKeyPolicy
	}
	if asyncVideo {
		req = req.WithContext(service.WithHTTPUpstreamRedirectsDisabled(req.Context()))
	}
	ctx, release, err := s.AcquireUpstreamKeySlots(req.Context(), fingerprints)
	if err != nil {
		return req, nil, err
	}
	if release == nil {
		return req, func(*http.Response, error) {}, nil
	}
	if asyncVideo {
		release()
		return req, nil, service.ErrUpstreamKeyPolicy
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
