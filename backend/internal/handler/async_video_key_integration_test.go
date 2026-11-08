//go:build unit && integration

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type failOnceAsyncVideoRelease struct{ fail atomic.Bool }

func (*failOnceAsyncVideoRelease) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*failOnceAsyncVideoRelease) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *failOnceAsyncVideoRelease) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "zrem" && h.fail.CompareAndSwap(true, false) {
			return errors.New("injected video release failure")
		}
		return next(ctx, cmd)
	}
}

func TestGrokVideoHandlerSharedKeyLifecycleWithRedis(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker and the local redis:7-alpine image are required")
	}
	container, err := exec.Command(docker, "run", "-d", "--rm", "--pull=never", "-p", "127.0.0.1::6379", "redis:7-alpine",
		"redis-server", "--appendonly", "yes", "--appendfsync", "always", "--no-appendfsync-on-rewrite", "no", "--maxmemory-policy", "noeviction").Output()
	if err != nil {
		t.Skipf("isolated Redis container did not start: %v", err)
	}
	containerID := strings.TrimSpace(string(container))
	t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", containerID).Run() })
	port, err := exec.Command(docker, "port", containerID, "6379/tcp").Output()
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{Addr: strings.TrimSpace(string(port)), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	require.Eventually(t, func() bool { return rdb.Ping(t.Context()).Err() == nil }, 10*time.Second, 50*time.Millisecond)

	var creates, polls, competing atomic.Int32
	var terminal atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/competing" {
			competing.Add(1)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		if r.Method == http.MethodPost {
			creates.Add(1)
			_, _ = io.WriteString(w, `{"request_id":"task","status":"pending"}`)
			return
		}
		polls.Add(1)
		if terminal.Load() {
			_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://vidgen.x.ai/result.mp4","duration":6}}`)
		} else {
			_, _ = io.WriteString(w, `{"status":"pending"}`)
		}
	}))
	t.Cleanup(provider.Close)

	h, slots, bindings, _ := newGrokMediaSlotHandler(t, false, false)
	h.cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	h.cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1,
		Limits: map[string]int{config.UpstreamKeyFingerprint("async-fixture"): 1}}
	account := service.Account{ID: 1, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{24},
		Credentials: map[string]any{"api_key": "async-fixture", "base_url": provider.URL}}
	repo := grokMediaSlotRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}}
	transport := repository.ProvideHTTPUpstream(h.cfg, nil, rdb, nil)
	concurrency := service.NewConcurrencyService(slots)
	h.gatewayService = service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, bindings, h.cfg, nil, concurrency, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)

	create, created := grokMediaSlotContext(t.Context(), true)
	h.GrokVideoGeneration(create)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	require.EqualValues(t, 1, creates.Load(), "the handler must send one real creation")
	slots.assertReleased(t)

	tryCompeting := func() error {
		req, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, provider.URL+"/competing", strings.NewReader("{}"))
		require.NoError(t, requestErr)
		req.Header.Set("Authorization", "Bearer async-fixture")
		response, sendErr := transport.Do(req, "", 2, 1)
		if response != nil {
			require.NoError(t, response.Body.Close())
		}
		return sendErr
	}
	require.ErrorIs(t, tryCompeting(), service.ErrUpstreamKeyCapacity)
	require.Zero(t, competing.Load())

	// The Redis owner, not an old sticky cache entry, must recover the account.
	bindings.owner, bindings.key = 0, ""
	wrong, denied := grokMediaSlotContext(context.Background(), false)
	groupID := int64(24)
	wrong.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 21, UserID: 11, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformGrok, AllowImageGeneration: true}, User: &service.User{ID: 11}})
	wrong.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 11, Concurrency: 5})
	h.GrokVideoStatus(wrong)
	require.Equal(t, http.StatusNotFound, denied.Code, denied.Body.String())
	require.Zero(t, polls.Load())

	status, pending := grokMediaSlotContext(t.Context(), false)
	h.GrokVideoStatus(status)
	require.Equal(t, http.StatusOK, pending.Code, pending.Body.String())
	require.EqualValues(t, 1, polls.Load(), "the owner must poll even at Key capacity")
	require.ErrorIs(t, tryCompeting(), service.ErrUpstreamKeyCapacity)
	terminal.Store(true)
	status, completed := grokMediaSlotContext(t.Context(), false)
	h.GrokVideoStatus(status)
	require.Equal(t, http.StatusOK, completed.Code, completed.Body.String())
	require.EqualValues(t, 2, polls.Load())
	require.NoError(t, tryCompeting(), "terminal status must release the durable member")
	require.EqualValues(t, 1, competing.Load())
	slots.assertReleased(t)

	terminal.Store(false)
	create, created = grokMediaSlotContext(t.Context(), true)
	h.GrokVideoGeneration(create)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	require.EqualValues(t, 2, creates.Load())
	hook := &failOnceAsyncVideoRelease{}
	hook.fail.Store(true)
	rdb.AddHook(hook)
	terminal.Store(true)
	status, completed = grokMediaSlotContext(t.Context(), false)
	h.GrokVideoStatus(status)
	require.Equal(t, http.StatusOK, completed.Code, completed.Body.String())
	require.False(t, hook.fail.Load(), "the terminal release must encounter the injected failure")
	require.EqualValues(t, 1, rdb.ZCard(t.Context(), "concurrency:upstream_key:async_video:release").Val())
	require.ErrorIs(t, tryCompeting(), service.ErrUpstreamKeyCapacity)
	h.gatewayService.StartAsyncVideoKeyReleaseRecovery()
	t.Cleanup(h.gatewayService.StopAsyncVideoKeyReleaseRecovery)
	require.Eventually(t, func() bool { return tryCompeting() == nil }, 5*time.Second, 50*time.Millisecond,
		"the background worker must return capacity after a transient terminal release failure")
	require.EqualValues(t, 2, competing.Load())
	require.Zero(t, rdb.ZCard(t.Context(), "concurrency:upstream_key:async_video:release").Val())
	slots.assertReleased(t)
}
