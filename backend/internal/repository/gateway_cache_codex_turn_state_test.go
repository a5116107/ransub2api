package repository

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestCodexTurnStateOriginSharedTTLAndInvalidRecords(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	reader := &gatewayCache{rdb: cache.rdb}
	key := strings.Repeat("a", 64)
	scope := service.OpenAIWSContinuationScope{AccountID: 42, RouteFingerprint: [32]byte{1}, Model: "gpt-5.3-codex", ServiceTier: "priority"}
	require.NoError(t, cache.SetOpenAICodexTurnStateOrigin(t.Context(), key, scope, time.Minute))
	got, found, err := reader.GetOpenAICodexTurnStateOrigin(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, scope, got)
	require.Equal(t, time.Minute, mr.TTL(codexTurnStateOriginPrefix+key))
	mr.FastForward(time.Minute)
	_, found, err = reader.GetOpenAICodexTurnStateOrigin(t.Context(), key)
	require.NoError(t, err)
	require.False(t, found)
	for _, bad := range []string{"", "raw-session", strings.Repeat("z", 64), strings.Repeat("a", 65)} {
		require.Error(t, cache.SetOpenAICodexTurnStateOrigin(t.Context(), bad, scope, time.Minute))
		_, _, err := reader.GetOpenAICodexTurnStateOrigin(t.Context(), bad)
		require.Error(t, err)
	}
	require.Error(t, cache.SetOpenAICodexTurnStateOrigin(t.Context(), key, scope, 0))
	for _, raw := range []string{`not-json`, `{}`, `{"version":99}`, `{"version":1,"scope":{"AccountID":42},"expires_at":"2099-01-01T00:00:00Z"}`} {
		require.NoError(t, mr.Set(codexTurnStateOriginPrefix+key, raw))
		_, found, err = reader.GetOpenAICodexTurnStateOrigin(t.Context(), key)
		require.Error(t, err)
		require.False(t, found)
	}
	data, err := json.Marshal(codexTurnStateOriginRecord{Version: 1, Scope: scope, ScopeHash: codexTurnStateScopeHash(scope)})
	require.NoError(t, err)
	require.NoError(t, mr.Set(codexTurnStateOriginPrefix+key, string(data)))
	mr.SetTTL(codexTurnStateOriginPrefix+key, 0)
	_, found, err = reader.GetOpenAICodexTurnStateOrigin(t.Context(), key)
	require.Error(t, err)
	require.False(t, found, "a record without a bounded Redis lifetime is invalid")
	mr.Del(codexTurnStateOriginPrefix + key)
	require.NoError(t, cache.SetOpenAICodexTurnStateOrigin(t.Context(), key, scope, time.Minute))
	mr.SetError("fixture read/write failure")
	_, _, err = reader.GetOpenAICodexTurnStateOrigin(t.Context(), key)
	require.Error(t, err)
	require.Error(t, cache.SetOpenAICodexTurnStateOrigin(t.Context(), key, scope, time.Minute))
	mr.SetError("")
	_, found, err = reader.GetOpenAICodexTurnStateOrigin(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
}

func TestCodexTurnStateOriginUsesRedisLifetime(t *testing.T) {
	for _, operation := range []string{"read", "conflicting_write", "missing_ttl"} {
		t.Run(operation, func(t *testing.T) {
			cache, mr := newHTTPResponseOwnerTestCache(t)
			key := strings.Repeat("c", 64)
			scope := service.OpenAIWSContinuationScope{AccountID: 42, RouteFingerprint: [32]byte{1}, Model: "gpt-5.4"}
			require.NoError(t, cache.SetOpenAICodexTurnStateOrigin(t.Context(), key, scope, time.Minute))
			data, err := mr.Get(codexTurnStateOriginPrefix + key)
			require.NoError(t, err)
			var record map[string]any
			require.NoError(t, json.Unmarshal([]byte(data), &record))
			// A writer's clock can be behind the reader while Redis still owns
			// a live record. Only the shared authority decides its lifetime.
			record["expires_at_ms"] = time.Now().Add(-time.Hour).UnixMilli()
			encoded, err := json.Marshal(record)
			require.NoError(t, err)
			require.NoError(t, mr.Set(codexTurnStateOriginPrefix+key, string(encoded)))
			mr.SetTTL(codexTurnStateOriginPrefix+key, time.Minute)
			switch operation {
			case "read":
				got, found, err := cache.GetOpenAICodexTurnStateOrigin(t.Context(), key)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, scope, got)
			case "conflicting_write":
				scope.AccountID++
				require.Error(t, cache.SetOpenAICodexTurnStateOrigin(t.Context(), key, scope, time.Minute))
			case "missing_ttl":
				mr.SetTTL(codexTurnStateOriginPrefix+key, 0)
				_, found, err := cache.GetOpenAICodexTurnStateOrigin(t.Context(), key)
				require.Error(t, err)
				require.False(t, found)
			}
		})
	}
}

func TestCodexTurnStateGatewaysShareRedisProvenance(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	testCodexTurnStateGateways(t, client, mr.SetError)
}

func TestCodexTurnStateGatewaysRealRedis(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required for this isolated adapter check")
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
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 200 * time.Millisecond, ContextTimeoutEnabled: true})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Eventually(t, func() bool { return client.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
	testCodexTurnStateGateways(t, client, nil)
	// Match production's context-aware socket I/O, then stall this disposable
	// Redis to verify the lookup budget bounds a real blocked read.
	require.NoError(t, client.Do(t.Context(), "CLIENT", "PAUSE", 600).Err())
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	cache := &gatewayCache{rdb: client}
	_, _, err = cache.GetOpenAICodexTurnStateOrigin(ctx, strings.Repeat("a", 64))
	require.Error(t, err)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	require.Eventually(t, func() bool { return client.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
}

func testCodexTurnStateGateways(t *testing.T, client *redis.Client, setError func(string)) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	const header = "X-Codex-Turn-State"
	received := make(chan string, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get(header)
		if r.Header.Get("Upgrade") != "" {
			w.Header().Set(header, "ws-minted-fixture")
			conn, err := coderws.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept upstream WS: %v", err)
				return
			}
			defer func() { _ = conn.CloseNow() }()
			if _, _, err := conn.Read(r.Context()); err != nil {
				t.Errorf("read upstream WS request: %v", err)
				return
			}
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_ws_provenance","status":"completed","model":"gpt-5.3-codex","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)); err != nil {
				t.Errorf("write WS response: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(header, "http-minted-fixture")
		_, _ = io.WriteString(w, `{"id":"resp_http_provenance","object":"response","status":"completed","model":"gpt-5.3-codex","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	account := &service.Account{ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"base_url": upstream.URL, "api_key": "supplier-fixture"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	forward := func(ws bool, accountID int64, state, model, tier string) (string, string) {
		t.Helper()
		a := *account
		a.ID = accountID
		cfg := &config.Config{}
		cfg.Security.URLAllowlist.AllowPrivateHosts = true
		cfg.Security.URLAllowlist.AllowInsecureHTTP = true
		cfg.Gateway.OpenAIWS.Enabled = ws
		cfg.Gateway.OpenAIWS.APIKeyEnabled = ws
		cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
		cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
		cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 0
		opts := *client.Options()
		gatewayRedis := redis.NewClient(&opts)
		defer func() { require.NoError(t, gatewayRedis.Close()) }()
		svc := service.NewOpenAIGatewayService(&lifecycleAccountRepo{account: &a}, nil, nil, nil, nil, nil, nil, NewGatewayCache(gatewayRedis), cfg, nil, nil, nil, nil, nil, NewHTTPUpstream(cfg), nil, nil, nil, nil, nil, nil, nil, nil)
		defer svc.CloseOpenAIWSPool()
		defer svc.StopOpenAICodexTicketHarvester()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set("session_id", "private-client-session")
		c.Request.Header.Set(header, state)
		c.Set("api_key", &service.APIKey{ID: 7})
		body, err := json.Marshal(map[string]any{"model": model, "service_tier": tier, "input": "hello", "stream": false})
		require.NoError(t, err)
		result, err := svc.Forward(t.Context(), c, &a, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		select {
		case actual := <-received:
			return actual, c.Writer.Header().Get(header)
		case <-time.After(3 * time.Second):
			t.Fatal("gateway did not reach the real upstream")
			return "", ""
		}
	}
	_, minted := forward(false, 42, "", "gpt-5.3-codex", "")
	require.Equal(t, "http-minted-fixture", minted)
	for _, tc := range []struct {
		name    string
		ws      bool
		account int64
		model   string
		tier    string
		want    string
	}{
		{"http_same", false, 42, "gpt-5.3-codex", "", minted},
		{"http_foreign_account", false, 43, "gpt-5.3-codex", "", ""},
		{"ws_same", true, 42, "gpt-5.3-codex", "", minted},
		{"ws_foreign_account", true, 43, "gpt-5.3-codex", "", ""},
		{"ws_foreign_model", true, 42, "gpt-5.4", "", ""},
		{"ws_priority", true, 42, "gpt-5.3-codex", "priority", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual, _ := forward(tc.ws, tc.account, minted, tc.model, tc.tier)
			require.Equal(t, tc.want, actual)
		})
	}
	// Conflicting observations of the same fixture state must preserve its
	// original scope, including the reverse WS-to-HTTP bridge.
	_, wsMinted := forward(true, 42, "", "gpt-5.3-codex", "")
	require.Equal(t, "ws-minted-fixture", wsMinted)
	actual, _ := forward(false, 42, wsMinted, "gpt-5.3-codex", "")
	require.Equal(t, wsMinted, actual)
	keys, err := client.Keys(t.Context(), codexTurnStateOriginPrefix+"*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	for _, key := range keys {
		require.NotContains(t, key, "private-client-session")
		require.NotContains(t, key, "minted-fixture")
		require.Greater(t, client.PTTL(context.Background(), key).Val(), time.Duration(0))
	}
	if setError != nil {
		setError("fixture provenance unavailable")
		actual, withheld := forward(false, 42, minted, "gpt-5.3-codex", "")
		require.Empty(t, actual, "an unverified client echo is stripped before sending")
		require.Empty(t, withheld, "a failed provenance write cannot advertise a new client state")
		setError("")
		actual, recovered := forward(false, 42, minted, "gpt-5.3-codex", "")
		require.Equal(t, minted, actual)
		require.Equal(t, minted, recovered)
	}
	// Competing instances must not reassign the same opaque token's origin.
	cache := &gatewayCache{rdb: client}
	key := strings.Repeat("b", 64)
	results := make(chan service.OpenAIWSContinuationScope, 32)
	var workers sync.WaitGroup
	for worker := range 32 {
		workers.Go(func() {
			scope := service.OpenAIWSContinuationScope{AccountID: int64(42 + worker%2), RouteFingerprint: [32]byte{1}, Model: "gpt-5.3-codex"}
			if err := cache.SetOpenAICodexTurnStateOrigin(t.Context(), key, scope, time.Minute); err == nil {
				results <- scope
			}
		})
	}
	workers.Wait()
	close(results)
	winner, found, err := cache.GetOpenAICodexTurnStateOrigin(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	successes := 0
	for scope := range results {
		require.Equal(t, winner, scope)
		successes++
	}
	require.Equal(t, 16, successes, "only observations matching the live first origin may publish")
}
