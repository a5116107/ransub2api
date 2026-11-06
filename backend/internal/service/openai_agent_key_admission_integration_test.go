//go:build integration

package service_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
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

type agentIntegrationRoundTrip func(*http.Request) (*http.Response, error)

func (f agentIntegrationRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAIAgentKeySharesRealRedisCapacityAndRecovers(t *testing.T) {
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

	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.True(t, strings.HasPrefix(req.Header.Get("Authorization"), "AgentAssertion "))
		sends.Add(1)
		w.Header().Set("x-codex-primary-used-percent", "18")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		_, _ = io.WriteString(w, "data: fixture\n\n")
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	serverPort, err := strconv.Atoi(portText)
	require.NoError(t, err)
	proxyID := int64(1)
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	account := &service.Account{ID: 801, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"auth_mode": service.OpenAIAuthModeAgentIdentity, "agent_runtime_id": "runtime-integration",
			"agent_private_key": base64.StdEncoding.EncodeToString(der), "task_id": "task-integration"},
		Extra: map[string]any{"codex_5h_used_percent": 77.0}, ProxyID: &proxyID,
		Proxy: &service.Proxy{Protocol: "http", Host: host, Port: serverPort}}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	require.True(t, ok)
	proxyURL := account.Proxy.URL()
	client, err := httppool.GetClient(httppool.Options{ProxyURL: proxyURL, Timeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Second})
	require.NoError(t, err)
	original := client.Transport
	base, err := url.Parse(server.URL)
	require.NoError(t, err)
	client.Transport = agentIntegrationRoundTrip(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})
	t.Cleanup(func() { client.Transport = original; httppool.EvictProxyClients(proxyURL) })

	cfg := &config.Config{}
	fingerprint := config.UpstreamAgentKeyFingerprint(publicKey)
	cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{fingerprint: 1}}
	upstream := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
	gateway := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	svc := service.ProvideAccountUsageService(nil, nil, nil, nil, nil, nil, nil, nil, service.NewUsageCache(), nil, nil, gateway)
	authority, ok := upstream.(service.UpstreamKeyConcurrency)
	require.True(t, ok)
	_, release, err := authority.AcquireUpstreamKeySlots(t.Context(), []string{fingerprint})
	require.NoError(t, err)
	require.NotNil(t, release)
	_, err = svc.GetUsageForAccount(t.Context(), account, true)
	require.NoError(t, err)
	require.Zero(t, sends.Load(), "the Agent signing key is already full")
	release()
	require.Eventually(t, func() bool {
		_, queryErr := svc.GetUsageForAccount(t.Context(), account, true)
		return queryErr == nil && sends.Load() == 1
	}, 8*time.Second, 100*time.Millisecond, "the same signing key recovers after bounded local retry")
}
