package repository

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyCacheSubscriber_BlocksUntilContextCancellation(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer func() { _ = client.Close() }()
	cache := NewAPIKeyCache(client)
	ctx, cancel := context.WithCancel(context.Background())
	received := make(chan string, 1)
	returned := make(chan error, 1)
	go func() {
		returned <- cache.SubscribeAuthCacheInvalidation(ctx, func(value string) { received <- value })
	}()

	var value string
	require.Eventually(t, func() bool {
		require.NoError(t, client.Publish(context.Background(), authCacheInvalidateChannel, "hash").Err())
		select {
		case value = <-received:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, "hash", value)
	select {
	case err := <-returned:
		t.Fatalf("subscriber returned while connection was active: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-returned:
		require.True(t, errors.Is(err, context.Canceled))
	case <-time.After(time.Second):
		t.Fatal("subscriber did not stop after context cancellation")
	}
}

func TestAPIKeyCacheSubscriberReportsAutomaticResubscription(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &apiKeyCache{rdb: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ready := make(chan struct{}, 4)
	received := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- cache.subscribeAuthCacheInvalidation(ctx, func(key string) { received <- key }, func() { ready <- struct{}{} })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("subscriber cleanup: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("subscriber cleanup did not finish")
		}
	})
	awaitReady := func() {
		t.Helper()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("go-redis adapter did not report subscription acknowledgement")
		}
	}
	awaitReady()
	// Force an actual connection loss on the isolated fixture only. Restart
	// preserves the port, so the same PubSub call must reconnect by itself.
	server.Close()
	require.NoError(t, server.Restart())
	awaitReady()
	require.NoError(t, client.Publish(ctx, authCacheInvalidateChannel, "fixture-key-after-reconnect").Err())
	select {
	case key := <-received:
		require.Equal(t, "fixture-key-after-reconnect", key)
	case <-time.After(time.Second):
		t.Fatal("messages stopped after the automatic resubscription")
	}
	select {
	case err := <-done:
		done <- err // Keep cleanup's one join intact on assertion failure.
		t.Fatalf("subscriber returned instead of reconnecting: %v", err)
	default:
	}
}

func TestAPIKeyCacheSubscriberReturnsRuntimeRedisErrors(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &apiKeyCache{rdb: client}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("subscriber did not stop after cancellation")
			}
		}
	})
	go func() {
		done <- cache.subscribeAuthCacheInvalidation(ctx, func(string) {}, func() { ready <- struct{}{} })
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not acknowledge its initial subscription")
	}
	// A server-side error on an established pubsub socket must reach the
	// service retry loop instead of leaving an unsubscribed connection alive.
	server.SetError("NOPERM fixture subscription unavailable")
	select {
	case err := <-done:
		joined = true
		require.True(t, redis.HasErrorPrefix(err, "NOPERM"), "unexpected subscriber result: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("runtime Redis error was hidden from the subscription retry loop")
	}
}

func TestAPIKeyCacheSubscriberIdleHeartbeat(t *testing.T) {
	for _, responds := range []bool{true, false} {
		name := "responsive"
		if !responds {
			name = "unresponsive"
		}
		t.Run(name, func(t *testing.T) {
			server := miniredis.RunT(t)
			ping := make(chan struct{}, 1)
			server.Server().SetPreHook(func(_ *miniserver.Peer, command string, _ ...string) bool {
				if command != "PING" {
					return false
				}
				select {
				case ping <- struct{}{}:
				default:
				}
				return !responds
			})
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			cache := &apiKeyCache{rdb: client}
			ctx, cancel := context.WithCancel(context.Background())
			ready := make(chan struct{}, 2)
			received := make(chan string, 1)
			done := make(chan error, 1)
			joined := false
			t.Cleanup(func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("idle subscriber did not stop after cancellation")
					}
				}
			})
			go func() {
				done <- cache.subscribeAuthCacheInvalidation(ctx, func(value string) { received <- value }, func() { ready <- struct{}{} })
			}()
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("idle subscriber did not acknowledge its initial subscription")
			}
			select {
			case <-ping:
			case <-time.After(5 * time.Second):
				t.Fatal("idle subscription was not checked with a heartbeat")
			}
			if !responds {
				select {
				case err := <-done:
					joined = true
					var timeout net.Error
					require.ErrorAs(t, err, &timeout)
					require.True(t, timeout.Timeout())
					require.ErrorContains(t, err, "auth cache invalidation heartbeat")
				case <-time.After(6 * time.Second):
					t.Fatal("unresponsive subscription did not return to the retry loop")
				}
				return
			}
			require.NoError(t, client.Publish(ctx, authCacheInvalidateChannel, "after-idle").Err())
			select {
			case value := <-received:
				require.Equal(t, "after-idle", value)
			case <-time.After(time.Second):
				t.Fatal("subscription stopped delivering invalidations after an idle heartbeat")
			}
			select {
			case <-ready:
				t.Fatal("an idle heartbeat must not mark a new subscription epoch")
			case err := <-done:
				joined = true
				t.Fatalf("responsive subscriber stopped: %v", err)
			default:
			}
		})
	}
}

func TestAPIKeyCacheSubscriberCancelsPendingAcknowledgement(t *testing.T) {
	server := miniredis.RunT(t)
	subscribed := make(chan struct{}, 1)
	server.Server().SetPreHook(func(_ *miniserver.Peer, command string, _ ...string) bool {
		if command != "SUBSCRIBE" {
			return false
		}
		subscribed <- struct{}{}
		return true // Leave the acknowledgement pending on an open socket.
	})
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &apiKeyCache{rdb: client}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("pending subscriber did not stop after cancellation")
			}
		}
	})
	go func() {
		done <- cache.subscribeAuthCacheInvalidation(ctx, func(string) {}, func() { ready <- struct{}{} })
	}()
	select {
	case <-subscribed:
	case <-time.After(time.Second):
		t.Fatal("subscription command did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		joined = true
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("pending acknowledgement was not interrupted by cancellation")
	}
	select {
	case <-ready:
		t.Fatal("an unacknowledged subscription must not authorize L1 reuse")
	default:
	}
}
