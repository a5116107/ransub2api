//go:build unit

package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGrokVoiceSharedKeyRefusalReturns503BeforeUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		attempts int
	}{
		{"capacity", service.ErrUpstreamKeyCapacity, 3},
		{"policy", service.ErrUpstreamKeyPolicy, 3},
		{"lease", service.ErrUpstreamKeyLease, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, slots, _, _ := newGrokMediaSlotHandler(t, false, false)
			ctx, recorder := grokMediaSlotContext(t.Context(), false)
			ctx.Request.Header.Set("Connection", "Upgrade")
			ctx.Request.Header.Set("Upgrade", "websocket")
			attempts := 0
			h.serveGrokVoiceWebSocket(ctx, grokVoiceWebSocket{
				name: "realtime", apiName: "Realtime API",
				open: func(context.Context, *service.Account, string) (*service.GrokRealtimeUpstream, error) {
					attempts++
					return nil, tc.err
				},
			})
			require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "upstream_key_admission_unavailable")
			require.Equal(t, tc.attempts, attempts)
			slots.assertReleased(t)
		})
	}
}

func TestGrokVoiceAvailabilityRefusalReturns503BeforeUpgrade(t *testing.T) {
	for _, mode := range []string{"realtime", "tts_stream"} {
		t.Run(mode, func(t *testing.T) {
			h, slots, _, _ := newGrokMediaSlotHandler(t, false, false)
			c, recorder := grokMediaSlotContext(t.Context(), false)
			c.Request.Header.Set("Connection", "Upgrade")
			c.Request.Header.Set("Upgrade", "websocket")
			attempts := 0
			h.serveGrokVoiceWebSocket(c, grokVoiceWebSocket{
				name: mode, apiName: "Voice API",
				open: func(context.Context, *service.Account, string) (*service.GrokRealtimeUpstream, error) {
					attempts++
					return nil, &service.OpenAITurnAdmissionError{Reason: "availability_scope_unconfirmed"}
				},
			})
			require.Equal(t, 3, attempts)
			require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "admission_unavailable")
			slots.assertReleased(t)
		})
	}
}

func TestGrokVoiceAvailabilityRefusalRespectsForbiddenBudget(t *testing.T) {
	h, slots, _, _ := newGrokMediaSlotHandler(t, false, false)
	c, recorder := grokMediaSlotContext(t.Context(), false)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	attempts := 0
	h.serveGrokVoiceWebSocket(c, grokVoiceWebSocket{
		name: "realtime", apiName: "Realtime API",
		open: func(_ context.Context, account *service.Account, _ string) (*service.GrokRealtimeUpstream, error) {
			attempts++
			if attempts == 1 {
				account.Extra = map[string]any{"grok_skip_forbidden_pause": true}
				return nil, &service.GrokRealtimeDialError{StatusCode: http.StatusForbidden,
					ResponseBody: []byte(`{"error":"unclassified refusal"}`), Err: errors.New("fixture rejection")}
			}
			return nil, &service.OpenAITurnAdmissionError{Reason: "availability_scope_unconfirmed"}
		},
	})
	require.Equal(t, 2, attempts, "a local refusal must not bypass the ambiguous 403 budget")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "admission_unavailable")
	slots.assertReleased(t)
}

func TestGrokVoiceAvailabilityRefusalFollowedBySupplierFailure(t *testing.T) {
	h, slots, _, _ := newGrokMediaSlotHandler(t, false, false)
	c, recorder := grokMediaSlotContext(t.Context(), false)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	attempts := 0
	h.serveGrokVoiceWebSocket(c, grokVoiceWebSocket{
		name: "realtime", apiName: "Realtime API",
		open: func(context.Context, *service.Account, string) (*service.GrokRealtimeUpstream, error) {
			attempts++
			if attempts == 1 {
				return nil, &service.OpenAITurnAdmissionError{Reason: "availability_scope_unconfirmed"}
			}
			return nil, &service.GrokRealtimeDialError{StatusCode: http.StatusForbidden,
				ResponseBody: []byte(`{"error":{"code":"content_policy","message":"rejected"}}`), Err: errors.New("fixture rejection")}
		},
	})
	require.Equal(t, 2, attempts)
	require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "admission_unavailable")
	slots.assertReleased(t)
}

func TestGrokVoiceAvailabilityRefusalFollowedByCredentialFailure(t *testing.T) {
	h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false)
	accounts := make([]service.Account, 3)
	for i := range accounts {
		accounts[i] = service.Account{ID: int64(i + 1), Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 10, Priority: i, GroupIDs: []int64{24}}
	}
	accounts[0].Credentials = map[string]any{"api_key": "test-key"}
	repo := grokMediaSlotRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: accounts}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	concurrency := service.NewConcurrencyService(slots)
	h.gatewayService = service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, bindings, cfg, nil, concurrency,
		nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	c, recorder := grokMediaSlotContext(t.Context(), false)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	attempts := 0
	h.serveGrokVoiceWebSocket(c, grokVoiceWebSocket{
		name: "realtime", apiName: "Realtime API",
		open: func(context.Context, *service.Account, string) (*service.GrokRealtimeUpstream, error) {
			attempts++
			return nil, &service.OpenAITurnAdmissionError{Reason: "availability_scope_unconfirmed"}
		},
	})
	require.Equal(t, 1, attempts)
	require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "admission_unavailable")
	slots.assertReleased(t)
}

func TestGrokVoiceLocalRefusalFollowedBySupplierFailure(t *testing.T) {
	h, slots, _, _ := newGrokMediaSlotHandler(t, false, false)
	c, recorder := grokMediaSlotContext(t.Context(), false)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	attempts := 0
	h.serveGrokVoiceWebSocket(c, grokVoiceWebSocket{
		name: "realtime", apiName: "Realtime API",
		open: func(context.Context, *service.Account, string) (*service.GrokRealtimeUpstream, error) {
			attempts++
			if attempts == 1 {
				return nil, service.ErrUpstreamKeyCapacity
			}
			return nil, &service.GrokRealtimeDialError{StatusCode: http.StatusForbidden,
				ResponseBody: []byte(`{"error":{"code":"content_policy","message":"rejected"}}`), Err: errors.New("fixture rejection")}
		},
	})
	require.Equal(t, 2, attempts)
	require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "upstream_key_admission_unavailable")
	slots.assertReleased(t)
}

func TestGrokVoiceLocalRefusalRespectsForbiddenFailoverBudget(t *testing.T) {
	h, slots, _, _ := newGrokMediaSlotHandler(t, false, false)
	c, recorder := grokMediaSlotContext(t.Context(), false)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	attempts := 0
	h.serveGrokVoiceWebSocket(c, grokVoiceWebSocket{
		name: "realtime", apiName: "Realtime API",
		open: func(_ context.Context, account *service.Account, _ string) (*service.GrokRealtimeUpstream, error) {
			attempts++
			if attempts == 1 {
				account.Extra = map[string]any{"grok_skip_forbidden_pause": true}
				return nil, &service.GrokRealtimeDialError{StatusCode: http.StatusForbidden,
					ResponseBody: []byte(`{"error":"unclassified refusal"}`), Err: errors.New("fixture rejection")}
			}
			return nil, service.ErrUpstreamKeyCapacity
		},
	})
	require.Equal(t, 2, attempts, "an ambiguous 403 permits only one alternate account")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "upstream_key_admission_unavailable")
	slots.assertReleased(t)
}

func TestGrokVoiceLeaseLossRecordsObservedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"realtime", "tts"} {
		t.Run(mode, func(t *testing.T) {
			h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false)
			cfg := &config.Config{RunMode: config.RunModeStandard}
			cfg.Default.RateMultiplier = 1
			account := service.Account{ID: 1, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, Concurrency: 10, GroupIDs: []int64{24},
				Credentials: map[string]any{"api_key": "fixture-key"}}
			repo := grokMediaSlotRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}}
			usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
			billing := &compatStreamUsageBillingRepo{}
			concurrency := service.NewConcurrencyService(slots)
			h.gatewayService = service.NewOpenAIGatewayService(repo, nil, usage, billing, nil, nil, nil, bindings, cfg, nil, concurrency,
				service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			groupID := int64(24)
			key := &service.APIKey{ID: 20, UserID: 10, GroupID: &groupID, User: &service.User{ID: 10},
				Group: &service.Group{ID: groupID, Platform: service.PlatformGrok, RateMultiplier: 1}}
			router := gin.New()
			router.GET("/voice", func(c *gin.Context) {
				c.Set(string(middleware2.ContextKeyAPIKey), key)
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 10, Concurrency: 5})
				h.serveGrokVoiceWebSocket(c, grokVoiceWebSocket{
					name: mode, apiName: "Voice API",
					open: func(context.Context, *service.Account, string) (*service.GrokRealtimeUpstream, error) {
						return &service.GrokRealtimeUpstream{}, nil
					},
					proxy: func(context.Context, *coderws.Conn, *service.GrokRealtimeUpstream) (*service.OpenAIForwardResult, error) {
						if mode == "realtime" {
							return grokRealtimeBillingResult("fixture", time.Minute, true), service.ErrUpstreamKeyLease
						}
						return grokTTSStreamBillingResult(1000, time.Second), service.ErrUpstreamKeyLease
					},
				})
			})
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/voice", nil)
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			_, _, _ = client.Read(ctx)
			select {
			case log := <-usage.created:
				require.NotNil(t, log)
				require.Positive(t, log.ActualCost, "observed usage must retain its priced cost")
			case <-ctx.Done():
				t.Fatal("lease loss discarded observed voice usage")
			}
			select {
			case <-usage.created:
				t.Fatal("voice usage was recorded more than once")
			default:
			}
			billing.mu.Lock()
			commands := append([]*service.UsageBillingCommand(nil), billing.commands...)
			billing.mu.Unlock()
			require.Len(t, commands, 1, "one observed session must create one billing command")
			require.Positive(t, commands[0].BalanceCost)
			require.NotEmpty(t, commands[0].RequestID)
		})
	}
}
