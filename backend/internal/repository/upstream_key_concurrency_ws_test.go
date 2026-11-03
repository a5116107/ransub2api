package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type upstreamKeyWSAccounts struct {
	service.AccountRepository
	account *service.Account
}

func (r upstreamKeyWSAccounts) GetOpenAITurnAdmission(context.Context, int64) (*service.Account, *service.Account, error) {
	copy := *r.account
	return &copy, nil, nil
}

func TestUpstreamKeyNativeWebSocketSharedLifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 10
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	var requests, handshakes, httpCalls atomic.Int32
	started, finish := make(chan struct{}, 4), make(chan struct{}, 4)
	closed := make(chan struct{}, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			httpCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		handshakes.Add(1)
		incoming := make(chan []byte, 1)
		peerClosed := make(chan struct{})
		go func() {
			defer close(peerClosed)
			for {
				_, message, readErr := conn.Read(ctx)
				if readErr != nil {
					return
				}
				select {
				case incoming <- message:
				case <-ctx.Done():
					return
				}
			}
		}()
		for {
			select {
			case <-incoming:
				request := requests.Add(1)
				started <- struct{}{}
				select {
				case <-finish:
					message := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_native_%d","model":"gpt-5.5","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`, request)
					if err := conn.Write(ctx, coderws.MessageText, []byte(message)); err != nil {
						return
					}
				case <-peerClosed:
					closed <- struct{}{}
					return
				case <-ctx.Done():
					return
				}
			case <-peerClosed:
				return
			case <-ctx.Done():
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 8,
		Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": upstream.URL}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	httpPort := newHTTPUpstream(cfg, nil, rdb)
	svc := service.NewOpenAIGatewayService(upstreamKeyWSAccounts{account: account}, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, httpPort, nil, nil, nil, nil, nil, nil, nil, nil)
	key := "concurrency:upstream_key:slots:" + config.UpstreamKeyFingerprint("supplier-fixture")
	type outcome struct {
		result *service.OpenAIForwardResult
		err    error
	}
	forward := func(turnCtx context.Context) outcome {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(turnCtx)
		result, err := svc.Forward(turnCtx, c, account, []byte(`{"model":"gpt-5.5","input":"hello","stream":false}`))
		return outcome{result, err}
	}
	authority, ok := httpPort.(service.UpstreamKeyConcurrency)
	require.True(t, ok)
	_, releaseHTTP, err := authority.AcquireUpstreamKeySlots(ctx, []string{config.UpstreamKeyFingerprint("supplier-fixture")})
	require.NoError(t, err)
	require.NotNil(t, releaseHTTP)
	t.Cleanup(releaseHTTP)
	blocked := forward(ctx)
	require.Nil(t, blocked.result)
	var failover *service.UpstreamFailoverError
	require.ErrorAs(t, blocked.err, &failover)
	require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
	require.Zero(t, requests.Load(), "full HTTP authority must reject WS before response.create")
	releaseHTTP()
	for turn := 1; turn <= 4; turn++ {
		turnCtx, cancelTurn := context.WithCancel(ctx)
		outcomes := make(chan outcome, 1)
		go func() {
			outcomes <- forward(turnCtx)
		}()
		select {
		case <-started:
		case out := <-outcomes:
			cancelTurn()
			t.Fatalf("native WS did not start: result=%+v err=%v", out.result, out.err)
		case <-ctx.Done():
			cancelTurn()
			t.Fatal("native WS request did not start")
		}
		require.EqualValues(t, 1, rdb.ZCard(ctx, key).Val())
		other := newHTTPUpstream(cfg, nil, rdb)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1/responses", strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer supplier-fixture")
		response, admissionErr := other.Do(req, "", 99, 100)
		require.Nil(t, response)
		require.ErrorIs(t, admissionErr, service.ErrUpstreamKeyCapacity)
		require.Zero(t, httpCalls.Load())
		if turn <= 2 {
			finish <- struct{}{}
		} else if turn == 3 {
			cancelTurn()
		} else {
			require.NoError(t, rdb.Del(ctx, key).Err())
		}
		select {
		case out := <-outcomes:
			if turn <= 2 {
				require.NoError(t, out.err)
				require.NotNil(t, out.result)
				require.True(t, out.result.OpenAIWSMode)
				require.EqualValues(t, 3, out.result.Usage.InputTokens)
				require.EqualValues(t, 1, handshakes.Load(), "successful idle connection is reusable")
			} else {
				require.Error(t, out.err)
				if turn == 4 {
					require.ErrorIs(t, out.err, service.ErrUpstreamKeyLease)
				}
			}
		case <-ctx.Done():
			t.Fatal("native WS turn did not finish")
		}
		cancelTurn()
		require.Eventually(t, func() bool { return rdb.ZCard(ctx, key).Val() == 0 }, time.Second, 10*time.Millisecond)
		if turn > 2 {
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("cancellation or lease loss left the upstream executing")
			}
		}
	}
	require.EqualValues(t, 4, requests.Load(), "sent turns must never replay through HTTP")
	require.Zero(t, httpCalls.Load())
}

func TestUpstreamKeyWebSocketBridgeSharedLifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = service.OpenAIWSIngressModePassthrough
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	var requests atomic.Int32
	started := make(chan struct{}, 4)
	finish := make(chan struct{}, 4)
	canceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer supplier-fixture" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		request := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"pending\"}}\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		started <- struct{}{}
		select {
		case <-finish:
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"model\":\"gpt-5.1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n", request)
		case <-r.Context().Done():
			canceled <- struct{}{}
		}
	}))
	t.Cleanup(upstream.Close)
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 8,
		Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": upstream.URL}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	httpPort := newHTTPUpstream(cfg, nil, rdb)
	svc := service.NewOpenAIGatewayService(upstreamKeyWSAccounts{account: account}, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, httpPort, nil, nil, nil, nil, nil, nil, nil, nil)
	turns := make(chan *service.OpenAIForwardResult, 4)
	errors := make(chan error, 4)
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			errors <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, first, err := conn.Read(ctx)
		if err != nil {
			errors <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(ctx)
		hooks := &service.OpenAIWSIngressHooks{AfterTurn: func(_ int, result *service.OpenAIForwardResult, _ error) { turns <- result }}
		errors <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "supplier-fixture", first, hooks)
	}))
	t.Cleanup(ingress.Close)
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(ingress.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	key := "concurrency:upstream_key:slots:" + config.UpstreamKeyFingerprint("supplier-fixture")
	for turn := 1; turn <= 2; turn++ {
		require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"hello"}`)))
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("bridge did not send HTTP request")
		}
		require.EqualValues(t, 1, rdb.ZCard(ctx, key).Val())
		// A separate account/client using the same actual credential cannot
		// multiply capacity while the WS turn holds its HTTP response.
		other := newHTTPUpstream(cfg, nil, rdb)
		req, buildErr := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1/responses", strings.NewReader("{}"))
		require.NoError(t, buildErr)
		req.Header.Set("Authorization", "Bearer supplier-fixture")
		response, admissionErr := other.Do(req, "", 99, 100)
		require.Nil(t, response)
		require.ErrorIs(t, admissionErr, service.ErrUpstreamKeyCapacity)
		require.EqualValues(t, turn, requests.Load())
		finish <- struct{}{}
		for {
			_, event, readErr := conn.Read(ctx)
			require.NoError(t, readErr)
			if gjson.GetBytes(event, "type").String() == "response.completed" {
				break
			}
		}
		select {
		case result := <-turns:
			require.NotNil(t, result)
			require.Equal(t, "http", result.UpstreamProtocol)
			require.EqualValues(t, 3, result.Usage.InputTokens)
		case <-ctx.Done():
			t.Fatal("missing turn completion")
		}
		require.Zero(t, rdb.ZCard(ctx, key).Val(), "idle WS must release the shared slot")
	}
	// Cancel an active third turn, rather than just closing an idle socket.
	require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"cancel"}`)))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("third turn did not start")
	}
	require.EqualValues(t, 1, rdb.ZCard(ctx, key).Val())
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not reach the upstream HTTP request")
	}
	require.Eventually(t, func() bool { return rdb.ZCard(t.Context(), key).Val() == 0 }, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, conn.CloseNow())
	select {
	case err := <-errors:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("bridge cleanup did not finish")
	}
}
