package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type wsTimingFailingWriter struct {
	gin.ResponseWriter
	writeFailure bool
	flushFailure bool
}

func (w *wsTimingFailingWriter) Write(p []byte) (int, error) {
	if w.writeFailure {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(p)
}

func (w *wsTimingFailingWriter) FlushError() error {
	if w.flushFailure {
		return io.ErrClosedPipe
	}
	return FlushGatewayResponse(w.ResponseWriter)
}

func TestWSTurnTimingNativeIngressWithoutSmartProtocol(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			parsedFirstText := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				for turn := 1; ; turn++ {
					if _, _, err = conn.Read(r.Context()); err != nil {
						return
					}
					terminal := "completed"
					if turn == 3 {
						terminal = "incomplete"
					}
					cached, reasoning := 80, 20
					if turn == 2 {
						cached, reasoning = 0, 0
					}
					for index, raw := range []string{
						fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_timing_%d"}}`, turn),
						`{"type":"response.reasoning_summary_text.delta","delta":"private-thought"}`,
						`{"type":"response.output_text.delta","delta":"private-answer"}`,
						`{"type":"response.output_text.delta","delta":"private-answer"}`,
						fmt.Sprintf(`{"type":"response.%s","response":{"id":"resp_timing_%d","model":"gpt-5.1","status":"%s","output":[{"type":"message","content":[{"type":"output_text","text":"private-answer"}]}],"usage":{"input_tokens":100,"output_tokens":120,"input_tokens_details":{"cached_tokens":%d},"output_tokens_details":{"reasoning_tokens":%d}}}}`, terminal, turn, terminal, cached, reasoning),
					} {
						if index == 3 {
							// Downstream acknowledgement proves the first frame was
							// parsed before this wait; wire sleep alone permits buffering.
							select {
							case <-parsedFirstText:
							case <-r.Context().Done():
								return
							case <-time.After(3 * time.Second):
								return
							}
							time.Sleep(300 * time.Millisecond)
						}
						if err = conn.Write(r.Context(), coderws.MessageText, []byte(raw)); err != nil {
							return
						}
					}
				}
			}))
			defer upstream.Close()
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
			defer svc.CloseOpenAIWSPool()
			account := passthroughLifecycleAccount()
			account.Credentials["base_url"] = upstream.URL
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			results := make(chan *OpenAIForwardResult, 4)
			diagnostics := make(chan requesttiming.Snapshot, 4)
			hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, _ error) {
				results <- result
				if result != nil && result.requestTiming != nil {
					result.requestTiming.WhenFinished(func(s requesttiming.Snapshot) { diagnostics <- s })
				}
			}}
			gateway, ended := startPassthroughLifecycleServer(t, t.Context(), svc, account, hooks)
			defer gateway.Close()
			client := dialOpenAIWSExecutionScopeClient(t, gateway.URL, "")
			defer func() { _ = client.CloseNow() }()
			ids := map[string]bool{}
			for turn := 1; turn <= 3; turn++ {
				writeOpenAIWSExecutionScopeRequest(t, client, `{"type":"response.create","model":"gpt-5.1","stream":true,"input":[{"role":"user","content":"fixture"}]}`)
				acknowledged := false
				for {
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					_, raw, err := client.Read(ctx)
					cancel()
					require.NoError(t, err)
					var event struct {
						Type string `json:"type"`
					}
					require.NoError(t, json.Unmarshal(raw, &event))
					if event.Type == "response.output_text.delta" && !acknowledged {
						parsedFirstText <- struct{}{}
						acknowledged = true
					}
					if openAIWSPassthroughIsTerminalOutput(raw) {
						break
					}
				}
				select {
				case result := <-results:
					require.NotNil(t, result)
					require.NotNil(t, result.requestTiming, "ordinary WS paths must collect without smart hooks or POST middleware")
					require.Equal(t, fmt.Sprintf("resp_timing_%d", turn), result.RequestID)
					require.Equal(t, 120, result.Usage.OutputTokens)
				case <-time.After(3 * time.Second):
					t.Fatal("turn result missing")
				}
				select {
				case s := <-diagnostics:
					require.False(t, ids[s.TraceID])
					ids[s.TraceID] = true
					require.Equal(t, "ws_turn", s.TTFTMode)
					require.Len(t, s.Attempts, 1)
					fingerprint := AccountRouteFingerprint(account)
					require.Equal(t, hex.EncodeToString(fingerprint[:]), s.Attempts[0].RouteFingerprint)
					v := requesttiming.DeriveContentObservation(s)
					require.Equal(t, "typed_responses_ws", v.Source)
					require.NotNil(t, v.ReasoningFirstMS)
					require.NotNil(t, v.VisibleTextFirstMS)
					require.Nil(t, v.ResponseByteFirstMS)
					require.Equal(t, 2, *v.TextStream.Chunks)
					if turn < 3 {
						require.Equal(t, "observed", v.TextStream.Status)
						require.Equal(t, "observed", v.Cache.Status)
						require.Equal(t, turn == 1, *v.Cache.Hit)
						require.Equal(t, "estimated", v.OutputRate.Status)
						require.NotNil(t, v.OutputRate.TokensPerSecond)
						require.Equal(t, "physical_websocket_turn_responses_text_at_parser", v.OutputRate.Scope)
						require.Equal(t, v.OutputRate, *s.OutputRate)
					} else {
						require.Equal(t, "partial", v.TextStream.Status)
						require.Equal(t, "partial", v.Cache.Status)
						require.Equal(t, "partial", v.OutputRate.Status)
						require.Nil(t, v.Cache.ReadRatio)
						require.Nil(t, v.OutputRate.TokensPerSecond)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("finished turn diagnostic missing")
				}
			}
			require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
			select {
			case err := <-ended:
				require.NoError(t, err)
			case <-time.After(4 * time.Second):
				t.Fatal("gateway session did not finish")
			}
		})
	}
}

type wsTimingUsageRepo struct {
	openAIRecordUsageLogRepoStub
	collectors []*requesttiming.Collector
}

func (r *wsTimingUsageRepo) RecordRequestTiming(ctx context.Context, _ string, _ int64) {
	r.collectors = append(r.collectors, requesttiming.UsageCollector(ctx))
}

func TestWSTurnTimingUsageBindingSurvivesBillingContext(t *testing.T) {
	repo := &wsTimingUsageRepo{}
	svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.cfg.RunMode = config.RunModeSimple
	parent := requesttiming.New(time.Now(), 0)
	ctx := requesttiming.With(t.Context(), parent)
	for i := range 2 {
		capture := newOpenAIWSTurnTiming(&Account{ID: 3}, time.Now())
		result := &OpenAIForwardResult{RequestID: fmt.Sprintf("resp_timing_%d", i), Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 10, OutputTokens: 5}}
		finishOpenAIWSTurnTiming(capture, ctx, result, nil)
		require.NoError(t, svc.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 1}, User: &User{ID: 2}, Account: &Account{ID: 3}}))
		require.Len(t, repo.collectors, i+1)
		require.Same(t, capture.Collector, repo.collectors[i])
	}
	require.NotSame(t, repo.collectors[0], repo.collectors[1])
}

func TestWSTurnTimingHTTPToWSUsesPhysicalGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			capture := &openAIWSCaptureConn{readDelays: []time.Duration{0, 0, 300 * time.Millisecond, 0}, events: [][]byte{
				[]byte(`{"type":"response.reasoning_summary_text.delta","delta":"thought"}`),
				[]byte(`{"type":"response.output_text.delta","delta":"answer"}`),
				[]byte(`{"type":"response.output_text.delta","delta":"answer"}`),
				[]byte(`{"type":"response.completed","response":{"id":"resp_http_ws_timing","model":"gpt-5.1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":8},"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}}}}`),
			}}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: capture})
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
			account := passthroughLifecycleAccount()
			account.Extra = map[string]any{"responses_websockets_v2_enabled": true}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			parent := requesttiming.New(time.Now(), 0)
			ctx := requesttiming.With(t.Context(), parent)
			result, err := svc.Forward(ctx, c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":[{"role":"user","content":"fixture"}]}`, stream)))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.OpenAIWSMode)
			require.NotNil(t, result.requestTiming)
			result.requestTiming.WhenFinished(func(s requesttiming.Snapshot) {
				require.Len(t, s.Attempts, 1)
				require.Equal(t, "ws_turn", s.Attempts[0].Kind)
				v := requesttiming.DeriveContentObservation(s)
				require.NotNil(t, v.ReasoningFirstMS)
				require.NotNil(t, v.VisibleTextFirstMS)
				require.Nil(t, v.ResponseByteFirstMS)
				require.Equal(t, "observed", v.TextStream.Status)
				require.Equal(t, "observed", v.Cache.Status)
				require.InDelta(t, 0.8, *v.Cache.ReadRatio, 1e-9)
				require.Equal(t, "estimated", v.OutputRate.Status)
				require.NotNil(t, v.OutputRate.TokensPerSecond)
				require.Equal(t, "physical_websocket_turn_responses_text_at_parser", v.OutputRate.Scope)
			})
			require.Same(t, result.requestTiming, requesttiming.UsageCollector(requesttiming.WithUsageTurn(ctx, result.requestTiming)))
			require.Equal(t, "resp_http_ws_timing", result.RequestID)
			require.Equal(t, 120, result.Usage.OutputTokens)
		})
	}
}

func TestWSTurnTimingHTTPToWSDeliveryFailures(t *testing.T) {
	for _, mode := range []string{"stream_write", "stream_flush", "json_write"} {
		t.Run(mode, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: &openAIWSCaptureConn{events: [][]byte{
				[]byte(`{"type":"response.output_text.delta","delta":"answer"}`),
				[]byte(`{"type":"response.completed","response":{"id":"resp_delivery_failure","status":"completed","model":"gpt-5.1","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":8},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0}}}}`),
			}}})
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
			account := passthroughLifecycleAccount()
			account.Extra = map[string]any{"responses_websockets_v2_enabled": true}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Writer = &wsTimingFailingWriter{ResponseWriter: c.Writer, writeFailure: mode != "stream_flush", flushFailure: mode == "stream_flush"}
			result, err := svc.Forward(t.Context(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":[{"role":"user","content":"fixture"}]}`, mode != "json_write")))
			require.NoError(t, err, "preserve upstream usage drain and the existing forwarding return contract")
			require.NotNil(t, result)
			require.Equal(t, 5, result.Usage.OutputTokens)
			require.NotNil(t, result.requestTiming)
			result.requestTiming.WhenFinished(func(s requesttiming.Snapshot) {
				require.True(t, s.DownstreamError)
				v := requesttiming.DeriveContentObservation(s)
				require.NotEqual(t, "observed", v.TextStream.Status)
				require.Equal(t, "partial", v.Cache.Status)
				require.Equal(t, "partial", v.OutputRate.Status)
				require.Nil(t, v.Cache.ReadRatio)
				require.Nil(t, v.OutputRate.TokensPerSecond)
			})
		})
	}
}

func TestWSTurnTimingPassthroughDisconnectDoesNotPublishHealthyTurn(t *testing.T) {
	cfg := passthroughLifecycleConfig()
	upstream := newStagedPassthroughConn()
	svc := newPassthroughLifecycleService(cfg, upstream)
	diagnostics := make(chan requesttiming.Snapshot, 1)
	hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, _ error) {
		if result != nil && result.requestTiming != nil {
			result.requestTiming.WhenFinished(func(s requesttiming.Snapshot) { diagnostics <- s })
		}
	}}
	contexts := make(chan *gin.Context, 1)
	server, ended := startPassthroughLifecycleServerWithHooks(t, t.Context(), svc, passthroughLifecycleAccount(), func(c *gin.Context) *OpenAIWSIngressHooks {
		contexts <- c
		return hooks
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = client.CloseNow() }()
	requirePassthroughUpstreamWrite(t, upstream, time.Second)
	reader := openAIWSIngressReaderFromContext(<-contexts)
	require.NotNil(t, reader)
	upstream.Send(`{"type":"response.created","response":{"id":"resp_disconnected_timing"}}`)
	upstream.Send(`{"type":"response.output_text.delta","delta":"answer"}`)
	_, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	_, err = readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	// Client acknowledgement places the second parser delta after a real
	// output window, so an accidental success would expose cache and TPS too.
	time.Sleep(300 * time.Millisecond)
	upstream.Send(`{"type":"response.output_text.delta","delta":"answer"}`)
	_, err = readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.NoError(t, client.CloseNow())
	// Wait for observed disconnect, not a sleep racing the terminal write.
	select {
	case <-reader.done:
	case <-time.After(time.Second):
		t.Fatal("ingress reader did not observe client disconnect")
	}
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_disconnected_timing","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":8},"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}}}}`)
	select {
	case err := <-ended:
		require.NoError(t, err)
	case <-time.After(4 * time.Second):
		t.Fatal("disconnect drain did not settle")
	}
	select {
	case s := <-diagnostics:
		v := requesttiming.DeriveContentObservation(s)
		require.NotEqual(t, "observed", v.TextStream.Status)
		require.Nil(t, v.Cache.ReadRatio)
		require.Nil(t, v.OutputRate.TokensPerSecond)
		require.True(t, s.ClientDisconnect || s.DownstreamError || s.Outcome != "success")
	case <-time.After(time.Second):
		t.Fatal("drained turn diagnostic missing")
	}
}
