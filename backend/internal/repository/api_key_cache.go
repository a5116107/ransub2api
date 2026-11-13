package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	apiKeyRateLimitKeyPrefix   = "apikey:ratelimit:"
	apiKeyRateLimitDuration    = 24 * time.Hour
	apiKeyCreateCountKeyPrefix = "apikey:create_count:"
	apiKeyAuthCachePrefix      = "apikey:auth:"
	authCacheInvalidateChannel = "auth:cache:invalidate"
)

// apiKeyRateLimitKey generates the Redis key for API key creation rate limiting.
func apiKeyRateLimitKey(userID int64) string {
	return fmt.Sprintf("%s%d", apiKeyRateLimitKeyPrefix, userID)
}

// apiKeyCreateCountKey generates the Redis key for per-user API key creation counting.
func apiKeyCreateCountKey(userID int64) string {
	return fmt.Sprintf("%s%d", apiKeyCreateCountKeyPrefix, userID)
}

func apiKeyAuthCacheKey(key string) string {
	return fmt.Sprintf("%s%s", apiKeyAuthCachePrefix, key)
}

type apiKeyCache struct {
	rdb *redis.Client
}

func NewAPIKeyCache(rdb *redis.Client) service.APIKeyCache {
	return &apiKeyCache{rdb: rdb}
}

func (c *apiKeyCache) GetCreateAttemptCount(ctx context.Context, userID int64) (int, error) {
	key := apiKeyRateLimitKey(userID)
	count, err := c.rdb.Get(ctx, key).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return count, err
}

func (c *apiKeyCache) IncrementCreateAttemptCount(ctx context.Context, userID int64) error {
	key := apiKeyRateLimitKey(userID)
	pipe := c.rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, apiKeyRateLimitDuration)
	_, err := pipe.Exec(ctx)
	return err
}

// IncrementCreateCount atomically increments a fixed-window counter. Only a
// missing TTL is repaired; later increments must not extend the window. Lua
// preserves the INCR/expiry boundary without Redis 7's EXPIRE NX option.
var incrementAPIKeyCreateCountScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if redis.call('TTL', KEYS[1]) == -1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
`)

func (c *apiKeyCache) IncrementCreateCount(ctx context.Context, userID int64, window time.Duration) (int64, error) {
	seconds := int64(window / time.Second)
	if window > 0 && seconds == 0 {
		seconds = 1
	}
	return incrementAPIKeyCreateCountScript.Run(ctx, c.rdb, []string{apiKeyCreateCountKey(userID)}, seconds).Int64()
}

func (c *apiKeyCache) IncrementDailyUsage(ctx context.Context, apiKey string) error {
	return c.rdb.Incr(ctx, apiKey).Err()
}

func (c *apiKeyCache) SetDailyUsageExpiry(ctx context.Context, apiKey string, ttl time.Duration) error {
	return c.rdb.Expire(ctx, apiKey, ttl).Err()
}

func (c *apiKeyCache) GetAuthCache(ctx context.Context, key string) (*service.APIKeyAuthCacheEntry, error) {
	val, err := c.rdb.Get(ctx, apiKeyAuthCacheKey(key)).Bytes()
	if err != nil {
		return nil, err
	}
	var entry service.APIKeyAuthCacheEntry
	if err := json.Unmarshal(val, &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

func (c *apiKeyCache) SetAuthCache(ctx context.Context, key string, entry *service.APIKeyAuthCacheEntry, ttl time.Duration) error {
	if entry == nil {
		return nil
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, apiKeyAuthCacheKey(key), payload, ttl).Err()
}

func (c *apiKeyCache) DeleteAuthCache(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, apiKeyAuthCacheKey(key)).Err()
}

// PublishAuthCacheInvalidation publishes a cache invalidation message to all instances
func (c *apiKeyCache) PublishAuthCacheInvalidation(ctx context.Context, cacheKey string) error {
	return c.rdb.Publish(ctx, authCacheInvalidateChannel, cacheKey).Err()
}

// SubscribeAuthCacheInvalidation subscribes to cache invalidation messages
func (c *apiKeyCache) SubscribeAuthCacheInvalidation(ctx context.Context, handler func(cacheKey string)) error {
	return c.subscribeAuthCacheInvalidation(ctx, handler, func() { service.NotifyAuthCacheSubscriptionReady(ctx) })
}

func (c *apiKeyCache) subscribeAuthCacheInvalidation(ctx context.Context, handler func(string), ready func()) error {
	pubsub := c.rdb.Subscribe(ctx, authCacheInvalidateChannel)
	var closeOnce sync.Once
	closeSubscription := func() {
		closeOnce.Do(func() {
			if err := pubsub.Close(); err != nil {
				log.Printf("Warning: failed to close auth cache invalidation pubsub: %v", err)
			}
		})
	}
	stopClosing := context.AfterFunc(ctx, closeSubscription)
	defer func() {
		stopClosing()
		closeSubscription()
	}()

	// Verify subscription is working
	_, err := pubsub.ReceiveTimeout(ctx, 5*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("subscribe to auth cache invalidation: %w", err)
	}
	ready()

	// ChannelWithSubscriptions hides protocol errors as well as connection
	// errors. A rejected resubscribe can leave a live but unsubscribed socket.
	// Surface server errors to the service retry loop; retain transport reconnects
	// and their acknowledgement checkpoints so missed L1 grants are discarded.
	readTimeout := 3 * time.Second
	waitingForPong := false
	for {
		event, err := pubsub.ReceiveTimeout(ctx, readTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var serverError redis.Error
			if errors.As(err, &serverError) || errors.Is(err, redis.ErrClosed) {
				return fmt.Errorf("receive auth cache invalidation: %w", err)
			}
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				if waitingForPong {
					return fmt.Errorf("auth cache invalidation heartbeat: %w", err)
				}
				if pingErr := pubsub.Ping(ctx); pingErr != nil {
					return fmt.Errorf("ping auth cache invalidation: %w", pingErr)
				}
				waitingForPong = true
				readTimeout = 5 * time.Second
				continue
			}
			if !errors.As(err, &networkError) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("receive auth cache invalidation: %w", err)
			}
			waitingForPong = false
			readTimeout = 3 * time.Second
			retry := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				retry.Stop()
				return ctx.Err()
			case <-retry.C:
			}
			continue
		}
		waitingForPong = false
		readTimeout = 3 * time.Second
		switch message := event.(type) {
		case *redis.Subscription:
			if message.Kind == "subscribe" && message.Channel == authCacheInvalidateChannel {
				ready()
			}
		case *redis.Message:
			if message != nil {
				handler(message.Payload)
			}
		}
	}
}
