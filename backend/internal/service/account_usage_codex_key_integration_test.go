//go:build integration

package service_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type codexIntegrationRoundTrip func(*http.Request) (*http.Response, error)

func (f codexIntegrationRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCodexUsageProbeSharesRealRedisCapacityAndRecovers(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required for this isolated integration")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	cmd := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--dir", t.TempDir(), "--save", "", "--appendonly", "no")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	rdb := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	require.Eventually(t, func() bool { return rdb.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)

	for _, wrap := range []bool{false, true} {
		name := "direct"
		if wrap {
			name = "astra"
		}
		t.Run(name, func(t *testing.T) {
			var probes, others atomic.Int32
			var slow atomic.Bool
			var auth atomic.Value
			started, allow := make(chan struct{}, 1), make(chan struct{})
			heldStop := make(chan struct{})
			var unblock sync.Once
			var stopHeld sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				switch req.URL.Path {
				case "/held":
					_, _ = io.WriteString(w, "held")
					if http.NewResponseController(w).Flush() != nil {
						return
					}
					select {
					case <-req.Context().Done():
					case <-heldStop:
					}
				case "/other":
					others.Add(1)
					_, _ = io.WriteString(w, "ok")
				default:
					probes.Add(1)
					auth.Store(req.Header.Get("Authorization"))
					if slow.Load() {
						started <- struct{}{}
						select {
						case <-allow:
						case <-req.Context().Done():
							return
						}
					}
					w.Header().Set("x-codex-primary-used-percent", "18")
					w.Header().Set("x-codex-primary-window-minutes", "300")
					_, _ = io.WriteString(w, "data: fixture\n\n")
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { unblock.Do(func() { close(allow) }) })
			t.Cleanup(func() { stopHeld.Do(func() { close(heldStop) }) })
			host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
			require.NoError(t, err)
			serverPort, err := strconv.Atoi(portText)
			require.NoError(t, err)
			proxyID := int64(1)
			account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "shared-fixture"},
				Extra:       map[string]any{"codex_5h_used_percent": 77.0},
				ProxyID:     &proxyID, Proxy: &service.Proxy{Protocol: "http", Host: host, Port: serverPort}}
			proxyURL := account.Proxy.URL()
			client, err := httppool.GetClient(httppool.Options{ProxyURL: proxyURL, Timeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Second})
			require.NoError(t, err)
			original := client.Transport
			base, err := url.Parse(server.URL)
			require.NoError(t, err)
			client.Transport = codexIntegrationRoundTrip(func(req *http.Request) (*http.Response, error) {
				copy := req.Clone(req.Context())
				copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
				return http.DefaultTransport.RoundTrip(copy)
			})
			t.Cleanup(func() { client.Transport = original; httppool.EvictProxyClients(proxyURL) })

			cfg := &config.Config{}
			cfg.Runtime.Role = config.RuntimeRoleGateway
			cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("shared-fixture"): 1}}
			cfg.Gateway.CodexGatewayPin.Enabled = wrap
			upstream := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
			gateway := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			t.Cleanup(gateway.CloseOpenAIWSPool)
			svc := service.ProvideAccountUsageService(nil, nil, nil, nil, nil, nil, nil, nil, service.NewUsageCache(), nil, nil, gateway)
			request := func(path string) *http.Request {
				req, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, strings.NewReader("{}"))
				require.NoError(t, requestErr)
				req.Header.Set("Authorization", "Bearer shared-fixture")
				return req
			}
			busy, err := upstream.Do(request("/held"), "", 4, 4)
			require.NoError(t, err)
			t.Cleanup(func() { _ = busy.Body.Close() })
			_, err = svc.GetUsageForAccount(t.Context(), account)
			require.NoError(t, err)
			require.Zero(t, probes.Load(), "full shared key must block the model probe before send")
			require.Equal(t, 77.0, account.Extra["codex_5h_used_percent"])
			require.NoError(t, busy.Body.Close())
			stopHeld.Do(func() { close(heldStop) })
			require.Eventually(t, func() bool {
				_, queryErr := svc.GetUsageForAccount(t.Context(), account, true)
				return queryErr == nil && probes.Load() == 1
			}, 8*time.Second, 100*time.Millisecond, "the same account retries after the bounded local cooldown")
			require.Equal(t, "Bearer shared-fixture", auth.Load())
			require.Equal(t, 18.0, account.Extra["codex_5h_used_percent"])

			// While a fresh probe waits for response headers, ordinary requests
			// with the same actual Bearer credential must share its occupied slot.
			slow.Store(true)
			second := *account
			second.Extra = map[string]any{}
			done := make(chan error, 1)
			go func() { _, queryErr := svc.GetUsageForAccount(t.Context(), &second, true); done <- queryErr }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("held model probe did not start")
			}
			other, err := upstream.Do(request("/other"), "", 5, 4)
			if other != nil {
				_ = other.Body.Close()
			}
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			require.Zero(t, others.Load())
			unblock.Do(func() { close(allow) })
			select {
			case err = <-done:
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("held probe did not finish")
			}
			other, err = upstream.Do(request("/other"), "", 5, 4)
			require.NoError(t, err)
			require.NoError(t, other.Body.Close())
			require.EqualValues(t, 1, others.Load())
		})
	}
}
