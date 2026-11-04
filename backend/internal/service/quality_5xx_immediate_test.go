//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/qualityqueue/queuetest"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type immediateQualityAccountRepo struct {
	AccountRepository
	calls             int
	availabilityCalls int
}

func TestAvailabilityProtocolMissingTerminalInvalidates(t *testing.T) {
	for _, path := range []string{"responses", "passthrough", "chat", "messages", "native_messages", "anthropic", "anthropic_passthrough", "anthropic_chat", "anthropic_responses", "ws_http_bridge"} {
		for _, async := range []bool{false, true} {
			for _, outcome := range []string{"empty", "partial", "complete", "canceled", "probe"} {
				t.Run(fmt.Sprintf("%s/async=%t/%s", path, async, outcome), func(t *testing.T) {
					repo := &immediateQualityAccountRepo{}
					limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
					cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
					if async {
						cfg.Gateway.StreamKeepaliveInterval = 60
					}
					svc := &OpenAIGatewayService{cfg: cfg, rateLimitService: limiter}
					account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					if outcome == "probe" {
						ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
					}
					payload := ""
					if outcome != "empty" {
						payload = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\",\"output_index\":0}\n\n"
					}
					if outcome == "complete" {
						payload += "data: " + controlledGatewayTerminal("target", "high") + "\n\n"
					}
					if path == "native_messages" || strings.HasPrefix(path, "anthropic") {
						payload = ""
						if outcome != "empty" {
							payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"target\",\"usage\":{\"input_tokens\":1}}}\n\n"
						}
						if outcome == "complete" {
							payload += "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
						}
					}
					if outcome == "canceled" {
						cancel()
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
					response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
					var err error
					switch path {
					case "responses":
						_, err = svc.handleStreamingResponse(ctx, response, c, account, time.Now(), "target", "target")
					case "passthrough":
						_, err = svc.handleStreamingResponsePassthrough(ctx, response, c, account, time.Now(), "target", "target")
					case "chat":
						_, err = svc.handleChatStreamingResponse(response, c, account, "target", "target", "target", time.Now(), 0)
					case "messages":
						_, err = svc.handleAnthropicStreamingResponse(response, c, account, "target", "target", "target", time.Now())
					case "native_messages":
						_, err = svc.handleNativeAnthropicStreamingResponse(ctx, response, c, account, "target", "target", "target", nil, time.Now())
					case "ws_http_bridge":
						svc.httpUpstream = &httpUpstreamRecorder{resp: response}
						request := []byte(`{"type":"response.create","model":"target","input":"fixture"}`)
						_, err = svc.proxyOpenAIWSHTTPBridgeTurn(ctx, c, account, "fixture", request, len(request), "target", "", "", "", "", 1, func([]byte) error { return nil })
					case "anthropic":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleStreamingResponse(ctx, response, c, account, time.Now(), "target", "target", false)
					case "anthropic_passthrough":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, response, c, account, time.Now(), "target")
					case "anthropic_chat":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleCCStreamingFromAnthropic(response, c, account, "target", "target", nil, time.Now())
					case "anthropic_responses":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleResponsesStreamingResponse(response, c, account, "target", "target", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
					}
					require.NoError(t, response.Body.Close())
					want := 0
					if outcome == "empty" || outcome == "partial" {
						// Legacy Anthropic converters treat an empty preamble as
						// unobserved, not an incomplete started message.
						if outcome == "empty" && (path == "anthropic_chat" || path == "anthropic_responses") {
							require.Zero(t, repo.availabilityCalls)
							return
						}
						want = 1
						require.Error(t, err)
					}
					if outcome == "complete" {
						require.NoError(t, err)
					}
					require.Equal(t, want, repo.availabilityCalls, "only protocol-confirmed incomplete responses should invalidate")
					require.Zero(t, repo.calls, "protocol incompleteness must not fabricate a quality 5xx")
				})
			}
		}
	}
}

func TestAvailabilityProtocolDisconnectExclusions(t *testing.T) {
	repo := &immediateQualityAccountRepo{}
	limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	limiter.observeAvailabilityIncomplete(t.Context(), &Account{ID: 42, Type: AccountTypeAPIKey}, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	limiter.observeAvailabilityIncomplete(ctx, &Account{ID: 42, Type: AccountTypeAPIKey}, false)
	require.Zero(t, repo.availabilityCalls)
}

func (r *immediateQualityAccountRepo) ApplyAvailabilityFault(ctx context.Context, id int64) error {
	r.availabilityCalls++
	return nil
}

func TestAvailabilityNon5xxFaultProvider(t *testing.T) {
	for _, status := range []int{401, 408, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := queuetest.NewClient(mr.Addr())
			t.Cleanup(func() { _ = rdb.Close() })
			repo := &immediateQualityAccountRepo{}
			svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, rdb)
			t.Cleanup(func() { _ = svc.qualityTrigger.queue.Close() })
			account := &Account{ID: 42, Type: AccountTypeAPIKey}
			svc.observeQualityResponse(t.Context(), account, &http.Response{StatusCode: status}, nil)
			require.Equal(t, 1, repo.availabilityCalls, "the observed failure must invalidate availability before returning")
			require.Zero(t, repo.calls, "a non-5xx fault must not invoke quality 5xx protection")
			require.False(t, mr.Exists(quality5xxPendingKey), "non-5xx faults use the durable due time, not a mislabeled quality event")
			svc.observeQualityResponse(t.Context(), account, &http.Response{StatusCode: status}, context.Canceled)
			svc.observeQualityStatus(context.WithValue(t.Context(), qualityProbeContextKey{}, true), account, status)
			for _, excluded := range []int{200, 400, 403, 404, 409, 422, 600} {
				svc.observeQualityStatus(t.Context(), account, excluded)
			}
			require.Equal(t, 1, repo.availabilityCalls)
		})
	}
}

func TestAvailabilityNon5xxFaultWithoutRedis(t *testing.T) {
	repo := &immediateQualityAccountRepo{}
	svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, status := range []int{401, 408, 429} {
			svc.observeQualityStatus(t.Context(), &Account{ID: 42, Type: accountType}, status)
		}
	}
	require.Equal(t, 6, repo.availabilityCalls)
	require.Zero(t, repo.calls)
	for _, account := range []*Account{nil, {ID: 0, Type: AccountTypeAPIKey}, {ID: 42, Type: AccountTypeSetupToken}, {ID: 42, Type: AccountTypeUpstream}} {
		svc.observeQualityStatus(t.Context(), account, 429)
	}
	require.Equal(t, 6, repo.availabilityCalls)
}

func TestAvailabilityResponseBodyFaultClassification(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		err                  error
		status               int
		cancel, close, probe bool
		want                 int
	}{
		{name: "normal EOF", err: io.EOF, status: 200},
		{name: "truncated body", err: io.ErrUnexpectedEOF, status: 200, want: 1},
		{name: "read failure", err: errors.New("fixture socket read failure"), status: 200, want: 1},
		{name: "upstream read timeout", err: context.DeadlineExceeded, status: 200, want: 1},
		{name: "read canceled", err: context.Canceled, status: 200},
		{name: "local capacity", err: ErrUpstreamKeyCapacity, status: 200},
		{name: "local key policy", err: ErrUpstreamKeyPolicy, status: 200},
		{name: "local turn admission", err: denyOpenAITurn("fixture"), status: 200},
		{name: "caller canceled", err: io.ErrUnexpectedEOF, status: 200, cancel: true},
		{name: "explicit cleanup", err: io.ErrUnexpectedEOF, status: 200, close: true},
		{name: "probe cannot recurse", err: io.ErrUnexpectedEOF, status: 200, probe: true},
		{name: "401 not counted twice", err: io.ErrUnexpectedEOF, status: 401, want: 1},
		{name: "policy response", err: io.ErrUnexpectedEOF, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &immediateQualityAccountRepo{}
			svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.probe {
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
			}
			response := &http.Response{StatusCode: tc.status, Body: io.NopCloser(iotest.ErrReader(tc.err))}
			svc.observeQualityResponse(ctx, &Account{ID: 42, Type: AccountTypeAPIKey}, response, nil)
			if tc.cancel {
				cancel()
			}
			if tc.close {
				require.NoError(t, response.Body.Close())
			}
			for range 2 {
				_, err := response.Body.Read(make([]byte, 1))
				require.ErrorIs(t, err, tc.err, "the observer must preserve the read error")
			}
			require.NoError(t, response.Body.Close())
			require.Equal(t, tc.want, repo.availabilityCalls, "one response must invalidate at most once")
			require.Zero(t, repo.calls)
		})
	}
}

func TestAvailabilityResponseBodyRealHTTPTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12")
		if _, err := w.Write([]byte("partial")); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	repo := &immediateQualityAccountRepo{}
	svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	svc.observeQualityResponse(t.Context(), &Account{ID: 42, Type: AccountTypeAPIKey}, response, nil)
	require.Zero(t, repo.availabilityCalls, "successful headers alone must not report a fault")
	content, err := io.ReadAll(response.Body)
	require.Equal(t, "partial", string(content))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, 1, repo.availabilityCalls, "an actual incomplete HTTP body must invalidate before the read returns")
	require.NoError(t, response.Body.Close())
	require.Equal(t, 1, repo.availabilityCalls)
}

func TestAvailabilitySIWCFaultObservation(t *testing.T) {
	for _, status := range []int{401, 408, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			repo := &immediateQualityAccountRepo{}
			limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			account := ticketTestAccount(901)
			account.Extra = map[string]any{OpenAISIWCEnabledKey: true}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: make(http.Header), Body: http.NoBody}}
			svc := &OpenAIGatewayService{rateLimitService: limiter, httpUpstream: upstream}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(`{"model":"target","input":"fixture"}`))
			require.NoError(t, err)
			response, err := svc.doOpenAIUpstream(request, "", account)
			require.NoError(t, err)
			require.Equal(t, status, response.StatusCode)
			require.NoError(t, response.Body.Close())
			require.Len(t, upstream.requests, 1)
			if status == 503 {
				require.Equal(t, 1, repo.calls)
				require.Zero(t, repo.availabilityCalls)
			} else {
				require.Equal(t, 1, repo.availabilityCalls)
				require.Zero(t, repo.calls)
			}
		})
	}
}

func (r *immediateQualityAccountRepo) ApplyQuality5xx(ctx context.Context, id int64) error {
	r.calls++
	return nil
}
func TestQuality5xxProductionProviderAppliesBeforeReturning(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := queuetest.NewClient(mr.Addr())
	defer func() { _ = rdb.Close() }()
	repo := &immediateQualityAccountRepo{}
	svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, rdb)
	require.NotNil(t, svc.qualityTrigger)
	defer func() { _ = svc.qualityTrigger.queue.Close() }()
	svc.observeQualityStatus(context.Background(), &Account{ID: 12, Type: AccountTypeOAuth}, 503)
	require.Equal(t, 1, repo.calls, "protection must finish before observer returns")
	members, err := mr.ZMembers(quality5xxPendingKey)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	svc.observeQualityStatus(context.WithValue(context.Background(), qualityProbeContextKey{}, true), &Account{ID: 12, Type: AccountTypeOAuth}, 503)
	require.Equal(t, 1, repo.calls, "probes must not recursively start episodes")
	svc.observeQualityStatus(context.Background(), &Account{ID: 12, Type: AccountTypeOAuth}, 429)
	require.Equal(t, 1, repo.calls)
	svc.observeQualityStatus(context.Background(), &Account{ID: 43, Type: AccountTypeAPIKey}, 503)
	require.Equal(t, 2, repo.calls, "API Key episodes must be fenced before the observer returns")
	members, err = mr.ZMembers(quality5xxPendingKey)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"12", "43"}, members)
}
func TestQuality5xxPendingRemoveModelsNotCoveredByOlderFinish(t *testing.T) {
	p := pelicanPlan()
	now := time.Now()
	finished := now.Add(time.Second)
	p.LastRunAt = &finished
	p.PelicanConfig.Quality = &QualityPolicy{TriggerOnUpstream5xx: true, Action: QualityActionRemoveModel, RemoveModels: []string{"target"}}
	require.False(t, qualitySignalCovered(p, now), "old probe finishing after new signal must not acknowledge it")
}

func TestQuality5xxImmediateProtectionWithoutQueue(t *testing.T) {
	repo := &immediateQualityAccountRepo{}
	svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	require.NotNil(t, svc.qualityTrigger)
	trigger := svc.qualityTrigger
	account := &Account{ID: 42, Type: AccountTypeAPIKey}
	trigger.Observe(t.Context(), account, 503)
	require.Equal(t, 1, repo.calls, "missing Redis must not disable the durable fault fence")
	trigger.Observe(context.WithValue(t.Context(), qualityProbeContextKey{}, true), account, 503)
	trigger.Observe(t.Context(), account, 429)
	require.Equal(t, 1, repo.calls)
}

func TestQuality5xxWorkerProviderHasQueue(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := queuetest.NewClient(mr.Addr())
	defer func() { _ = rdb.Close() }()
	// Exercise provider wiring without starting singleton cron jobs with nil
	// repositories. Such jobs can outlive this test and panic on a later tick.
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	svc := ProvideScheduledTestRunnerService(nil, nil, &AccountTestService{}, &RateLimitService{}, cfg, &QualityJudgeService{}, rdb, nil, &ChannelMonitorV2Service{})
	t.Cleanup(svc.Stop)
	require.NotNil(t, svc.qualityTrigger, "worker provider must connect the cross-instance queue")
	require.Nil(t, svc.cron)
	require.Nil(t, svc.triggerCancel)
}
