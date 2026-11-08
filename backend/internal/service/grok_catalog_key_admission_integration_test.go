//go:build integration

package service_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGrokCatalogRealSharedKeyCapacityAndLeaseRecovery(t *testing.T) {
	rdb := startKeyAdmissionIntegrationRedis(t)
	var sends atomic.Int32
	var slow atomic.Bool
	started := make(chan struct{}, 1)
	stopHeld := make(chan struct{})
	stopSlow := make(chan struct{})
	var stopOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/held" {
			_, _ = io.WriteString(w, "held")
			_ = http.NewResponseController(w).Flush()
			select {
			case <-stopHeld:
			case <-r.Context().Done():
			}
			return
		}
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if slow.Load() {
			_, _ = io.WriteString(w, `{"data":`)
			_ = http.NewResponseController(w).Flush()
			started <- struct{}{}
			select {
			case <-stopSlow:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"grok-4.7"}]}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { stopOnce.Do(func() { close(stopHeld) }); close(stopSlow) })

	const key = "grok-catalog-fixture"
	fingerprint := config.UpstreamKeyFingerprint(key)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{fingerprint: 1}}
	transport := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
	svc := service.NewAccountTestService(nil, nil, nil, nil, nil, transport, cfg, nil)
	account := &service.Account{ID: 41, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": key, "base_url": server.URL}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/held", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	busy, err := transport.Do(req, "", 42, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = busy.Body.Close() })
	_, err = svc.FetchUpstreamSupportedModels(t.Context(), account)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	require.True(t, service.IsUpstreamModelSyncLocalKeyFailure(err))
	require.Zero(t, sends.Load(), "a full shared Key must stop the model request before network send")
	require.NoError(t, busy.Body.Close())
	stopOnce.Do(func() { close(stopHeld) })
	models, err := svc.FetchUpstreamSupportedModels(t.Context(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"grok-4.7"}, models)

	// Keep a second response open until its Redis member is removed, simulating
	// an actual lost lease during body consumption, then verify recovery.
	slow.Store(true)
	done := make(chan error, 1)
	go func() { _, fetchErr := svc.FetchUpstreamSupportedModels(t.Context(), account); done <- fetchErr }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model response did not begin")
	}
	slotKey := "concurrency:upstream_key:slots:" + fingerprint
	members, err := rdb.ZRange(t.Context(), slotKey, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, members, 1)
	removed, err := rdb.ZRem(t.Context(), slotKey, members[0]).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	select {
	case err = <-done:
		require.ErrorIs(t, err, service.ErrUpstreamKeyLease)
		require.True(t, service.IsUpstreamModelSyncLocalKeyFailure(err))
	case <-time.After(12 * time.Second):
		t.Fatal("lost shared Key lease did not stop the model response")
	}
	slow.Store(false)
	models, err = svc.FetchUpstreamSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"grok-4.7"}, models)
	require.EqualValues(t, 3, sends.Load())
}
