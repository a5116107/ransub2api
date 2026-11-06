package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type bpsScopeFixtureUpstream struct{ server *httptest.Server }

func (u *bpsScopeFixtureUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = "http"
	cloned.URL.Host = strings.TrimPrefix(u.server.URL, "http://")
	cloned.Host = cloned.URL.Host
	return u.server.Client().Do(cloned)
}

func (u *bpsScopeFixtureUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestBPSSharedScopeAcrossGateways(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	testBPSSharedScopeAcrossGateways(t, client, server.SetError)
}

func TestBPSSharedScopeAcrossGatewaysRealRedis(t *testing.T) {
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
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Eventually(t, func() bool { return client.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
	testBPSSharedScopeAcrossGateways(t, client, nil)
}

func testBPSSharedScopeAcrossGateways(t *testing.T, client *redis.Client, setError func(string)) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var sends atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read BPS fixture request: %v", err)
			return
		}
		attempt := sends.Add(1)
		output := []any{}
		if attempt == 1 {
			args, marshalErr := json.Marshal(map[string]any{"summary": "Run client tool", "code": `{"name":"shell","arguments":{"cmd":"fixture"}}`,
				"extended_summary": "{}", "references": []any{}, "destructive": false})
			if marshalErr != nil {
				t.Errorf("marshal BPS fixture call: %v", marshalErr)
				return
			}
			output = append(output, map[string]any{"type": "function_call", "name": "run_officejs", "id": "fc_shared", "call_id": "call_shared", "arguments": string(args), "status": "completed"})
		} else if !bytes.Contains(body, []byte("run_officejs")) {
			t.Error("same-route continuation must include the original native tool call")
		}
		event, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_shared", "status": "completed", "model": "gpt-5.6-sol", "output": output,
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
		}})
		if err != nil {
			t.Errorf("marshal BPS fixture response: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", event); err != nil {
			t.Errorf("write BPS fixture response: %v", err)
		}
	}))
	t.Cleanup(upstream.Close)
	account := &service.Account{ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"access_token": "private-bps-fixture", "chatgpt_account_id": "private-account"},
		Extra:       map[string]any{"openai_excel_bps": true, "openai_passthrough": true}}
	cfg := &config.Config{}
	cfg.Gateway.ExcelBPSStateRedisEnabled = true
	transport := &bpsScopeFixtureUpstream{server: upstream}
	newGateway := func() *service.OpenAIGatewayService {
		svc := service.NewOpenAIGatewayService(&lifecycleAccountRepo{account: account}, nil, nil, nil, nil, nil, nil,
			NewGatewayCache(client), cfg, nil, nil, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)
		t.Cleanup(svc.StopOpenAICodexTicketHarvester)
		t.Cleanup(svc.CloseOpenAIWSPool)
		return svc
	}
	first, second := newGateway(), newGateway()
	forward := func(svc *service.OpenAIGatewayService, model string, input any) (*httptest.ResponseRecorder, error) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"model": model, "stream": false, "input": input,
			"tools": []any{map[string]any{"type": "function", "name": "shell"}}})
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Request.Header.Set("session_id", "private-shared-session")
		c.Set("api_key", &service.APIKey{ID: 7, UserID: 8})
		_, err = svc.Forward(t.Context(), c, account, body)
		return recorder, err
	}
	result, err := forward(first, "gpt-5.6-sol", "fixture")
	require.NoError(t, err)
	require.Contains(t, result.Body.String(), "call_shared")
	continuation := []any{map[string]any{"type": "function_call_output", "call_id": "call_shared", "output": "done"}}
	result, err = forward(second, "gpt-6-astra", continuation)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, result.Code)
	require.EqualValues(t, 1, sends.Load(), "a different model cannot replay a shared native item")
	if setError != nil {
		setError("synthetic shared-state outage")
		result, err = forward(second, "gpt-5.6-sol", continuation)
		require.Error(t, err)
		require.Equal(t, http.StatusServiceUnavailable, result.Code)
		require.EqualValues(t, 1, sends.Load(), "shared-state outage must fail before upstream send")
		setError("")
	}
	result, err = forward(second, "gpt-5.6-sol", continuation)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.Code)
	require.EqualValues(t, 2, sends.Load(), "another gateway may continue using the original scope")
	keys, err := client.Keys(t.Context(), bpsStatePrefix+"*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	for _, key := range keys {
		require.NotContains(t, key, "private-shared-session")
		require.NotContains(t, key, "private-bps-fixture")
		require.NotContains(t, key, "private-account")
	}
}

func TestBPSSharedStateCrossInstanceCASIsolationExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	first := redis.NewClient(&redis.Options{Addr: server.Addr()})
	second := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })
	a, b := &gatewayCache{rdb: first}, &gatewayCache{rdb: second}
	ctx := context.Background()
	scope := "account:1/key:2/thread:example"
	zero := uint64(0)
	ok, err := a.SaveBPSState(ctx, scope, "catalog", &zero, []byte("first"))
	require.NoError(t, err)
	require.True(t, ok)
	raw, version, err := b.LoadBPSState(ctx, scope, "catalog")
	require.NoError(t, err)
	require.Equal(t, "first", string(raw))
	require.NotZero(t, version)
	ok, err = b.SaveBPSState(ctx, scope, "catalog", &zero, []byte("stale"))
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = b.SaveBPSState(ctx, scope, "catalog", &version, []byte("new"))
	require.NoError(t, err)
	require.True(t, ok)
	for _, other := range []string{"account:2/key:2/thread:example", "account:1/key:3/thread:example", "account:1/key:2/thread:other"} {
		raw, _, err = a.LoadBPSState(ctx, other, "catalog")
		require.NoError(t, err)
		require.Empty(t, raw)
	}
	server.FastForward(time.Hour)
	_, _, err = b.LoadBPSState(ctx, scope, "catalog")
	require.NoError(t, err)
	require.Equal(t, bpsStateTTL, server.TTL(bpsStateKey(scope, "catalog")))
	server.FastForward(bpsStateTTL + time.Second)
	raw, _, err = a.LoadBPSState(ctx, scope, "catalog")
	require.NoError(t, err)
	require.Empty(t, raw)
}

func TestBPSSharedStateBudgetsAndErrors(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	c := &gatewayCache{rdb: client}
	ctx := context.Background()
	_, err := c.SaveBPSState(ctx, "scope", "large", nil, []byte(strings.Repeat("x", bpsStateEntryLimit+1)))
	require.ErrorIs(t, err, basispoints.ErrStateCapacity)
	for i := 0; i < 20; i++ {
		_, err = c.SaveBPSState(ctx, fmt.Sprint(i), "replay", nil, []byte(strings.Repeat("x", bpsStateEntryLimit-100)))
		require.NoError(t, err)
	}
	size, err := client.HGet(ctx, bpsStatePrefix+"sizes", "__bytes").Int64()
	require.NoError(t, err)
	require.LessOrEqual(t, size, int64(bpsStateByteLimit))
	count, err := client.ZCard(ctx, bpsStatePrefix+"lru").Result()
	require.NoError(t, err)
	require.LessOrEqual(t, count, int64(bpsStateCountLimit))
	// Closed client fails immediately; no live Redis or network outage timing needed.
	require.NoError(t, client.Close())
	_, _, err = c.LoadBPSState(ctx, "scope", "catalog")
	require.ErrorIs(t, err, basispoints.ErrStateUnavailable)
}
