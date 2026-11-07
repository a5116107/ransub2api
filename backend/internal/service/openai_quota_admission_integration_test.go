//go:build integration

package service_test

import (
	"context"
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
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/imroc/req/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type quotaIntegrationAccountRepo struct {
	service.AccountRepository
	account *service.Account
}

func redisServerVersion(info string) (int, string) {
	version := ""
	for _, line := range strings.Split(info, "\n") {
		if strings.HasPrefix(line, "redis_version:") {
			version = strings.TrimSpace(strings.TrimPrefix(line, "redis_version:"))
			break
		}
	}
	majorText, _, _ := strings.Cut(version, ".")
	major, _ := strconv.Atoi(majorText)
	return major, version
}

func (r *quotaIntegrationAccountRepo) GetByID(_ context.Context, _ int64) (*service.Account, error) {
	return r.account, nil
}

func startKeyAdmissionIntegrationRedis(t *testing.T) *redis.Client {
	t.Helper()
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
	info, infoErr := rdb.Info(t.Context(), "server").Result()
	if infoErr != nil {
		t.Skipf("redis server info is unavailable: %v", infoErr)
	}
	major, version := redisServerVersion(info)
	if major < 4 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		docker, dockerErr := exec.LookPath("docker")
		if dockerErr != nil {
			t.Skipf("redis-server %q lacks required scripting commands and Docker is unavailable", version)
		}
		container, runErr := exec.Command(docker, "run", "-d", "--rm", "-p", "127.0.0.1:"+port+":6379", "redis:7", "redis-server", "--bind", "0.0.0.0", "--port", "6379", "--save", "", "--appendonly", "no").Output()
		if runErr != nil {
			t.Skipf("redis-server %q lacks required scripting commands and Docker Redis 7 could not start: %v", version, runErr)
		}
		containerID := strings.TrimSpace(string(container))
		t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", containerID).Run() })
		require.Eventually(t, func() bool { return rdb.Ping(t.Context()).Err() == nil }, 10*time.Second, 100*time.Millisecond)
		info, infoErr = rdb.Info(t.Context(), "server").Result()
		if infoErr != nil {
			t.Skipf("Docker Redis server info is unavailable: %v", infoErr)
		}
		major, version = redisServerVersion(info)
		if major < 4 {
			t.Skipf("Docker Redis version %q lacks required scripting commands", version)
		}
	}
	return rdb
}

func TestOpenAIQuotaSharesRealRedisCapacityAndRecovers(t *testing.T) {
	rdb := startKeyAdmissionIntegrationRedis(t)
	for _, astra := range []bool{false, true} {
		name := "direct"
		if astra {
			name = "astra"
		}
		t.Run(name, func(t *testing.T) {
			var quotaSends, otherSends atomic.Int32
			var slow atomic.Bool
			started := make(chan struct{}, 1)
			finishBody, stopHeld := make(chan struct{}), make(chan struct{})
			var finishOnce, heldOnce sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/held":
					_, _ = io.WriteString(w, "held")
					_ = http.NewResponseController(w).Flush()
					select {
					case <-stopHeld:
					case <-r.Context().Done():
					}
				case "/other":
					otherSends.Add(1)
					_, _ = io.WriteString(w, "ok")
				default:
					quotaSends.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if slow.Load() && strings.HasSuffix(r.URL.Path, "/wham/usage") {
						_, _ = io.WriteString(w, "{")
						_ = http.NewResponseController(w).Flush()
						started <- struct{}{}
						select {
						case <-finishBody:
						case <-r.Context().Done():
							return
						}
						_, _ = io.WriteString(w, "}")
						return
					}
					_, _ = io.WriteString(w, `{}`)
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { finishOnce.Do(func() { close(finishBody) }); heldOnce.Do(func() { close(stopHeld) }) })
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			shared := req.C().WrapRoundTripFunc(func(next req.RoundTripper) req.RoundTripFunc {
				return func(r *req.Request) (*req.Response, error) {
					r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
					return next.RoundTrip(r)
				}
			})
			factory := func(string) (*req.Client, error) { return shared, nil }
			account := &service.Account{ID: 917, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "shared-fixture", "chatgpt_account_id": "quota-account"}}
			repo := &quotaIntegrationAccountRepo{account: account}
			cfg := &config.Config{}
			cfg.Runtime.Role = config.RuntimeRoleGateway
			cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("shared-fixture"): 1}}
			cfg.Gateway.CodexGatewayPin.Enabled = astra
			upstream := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
			gateway := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			t.Cleanup(gateway.CloseOpenAIWSPool)
			quota := service.ProvideOpenAIQuotaService(repo, nil, service.NewOpenAITokenProvider(repo, nil, nil), factory, nil, gateway)
			request := func(path string) *http.Request {
				r, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, strings.NewReader("{}"))
				require.NoError(t, requestErr)
				r.Header.Set("Authorization", "Bearer shared-fixture")
				return r
			}

			busy, err := upstream.Do(request("/held"), "", 1, 1)
			require.NoError(t, err)
			t.Cleanup(func() { _ = busy.Body.Close() })
			_, err = quota.QueryUsage(t.Context(), account.ID)
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			_, err = quota.ResetCreditTargeted(t.Context(), account.ID, "credit-fixture", "redeem-fixture")
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			require.Zero(t, quotaSends.Load())
			require.NoError(t, busy.Body.Close())
			heldOnce.Do(func() { close(stopHeld) })
			for i := 0; i < 2; i++ {
				_, err = quota.QueryUsage(t.Context(), account.ID)
				require.NoError(t, err, "repeated calls must not accumulate admissions")
			}
			_, err = quota.ResetCreditTargeted(t.Context(), account.ID, "credit-fixture", "redeem-fixture")
			require.NoError(t, err)
			require.EqualValues(t, 5, quotaSends.Load())

			slow.Store(true)
			done := make(chan error, 1)
			go func() { _, queryErr := quota.QueryUsage(t.Context(), account.ID); done <- queryErr }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("quota response body did not start")
			}
			other, err := upstream.Do(request("/other"), "", 2, 1)
			if other != nil {
				_ = other.Body.Close()
			}
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			require.Zero(t, otherSends.Load(), "quota response body must retain the shared slot")
			finishOnce.Do(func() { close(finishBody) })
			select {
			case queryErr := <-done:
				require.NoError(t, queryErr)
			case <-time.After(5 * time.Second):
				t.Fatal("quota query did not finish")
			}
			other, err = upstream.Do(request("/other"), "", 2, 1)
			require.NoError(t, err)
			require.NoError(t, other.Body.Close())
			require.EqualValues(t, 1, otherSends.Load())
		})
	}
}
