//go:build integration

package repository_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamOperationsHTTPPostgresWithoutModelChannelBinding(t *testing.T) {
	db := repository.ChannelOperationsTestDatabase()
	var account, user, key int64
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, db.QueryRow(`INSERT INTO accounts(name,platform,type,credentials,extra) VALUES('HTTP channel fixture','openai','apikey','{"base_url":"https://http.example:8443/tenant/v1/","api_key":"fixture-do-not-return"}','{"cost_multiplier":0.25}') RETURNING id`).Scan(&account))
	require.NoError(t, db.QueryRow(`INSERT INTO users(email,password_hash) VALUES($1,'fixture-hash') RETURNING id`, fmt.Sprintf("http-channel-%d@test.invalid", now.UnixNano())).Scan(&user))
	require.NoError(t, db.QueryRow(`INSERT INTO api_keys(user_id,key,name) VALUES($1,$2,'HTTP fixture') RETURNING id`, user, fmt.Sprintf("http-channel-fixture-%d", account)).Scan(&key))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM usage_logs WHERE account_id=$1`, account)
		_, _ = db.Exec(`DELETE FROM users WHERE id=$1`, user)
		_, _ = db.Exec(`DELETE FROM accounts WHERE id=$1`, account)
	})
	_, err := db.Exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,total_cost,actual_cost,account_stats_cost,account_rate_multiplier,created_at) VALUES($1,$2,$3,'http-channel-request','cache-model',10,4,8,999,$4)`, user, key, account, now.Add(-time.Hour))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO request_timing_details(usage_log_id,trace_id,detail)
 SELECT id,'00000000-0000-0000-0000-000000000001',jsonb_build_object('version',1,
 'raw_upstream',jsonb_build_object('version',1,'status','observed','account_id',account_id,'transport','http',
 'credential_source','http_transport_handoff','credential_fingerprint',repeat('a',64),'route_fingerprint',repeat('b',64)),
 'raw_cache',jsonb_build_object('status','observed','source','canonical_responses_terminal_usage',
 'scope','final_physical_http_responses_terminal_usage','input_tokens',100,'read_tokens',70,'write_tokens',NULL))
 FROM usage_logs WHERE account_id=$1`, account)
	require.NoError(t, err)
	var before string
	require.NoError(t, db.QueryRow(`SELECT row_to_json(u)::text FROM usage_logs u WHERE account_id=$1`, account).Scan(&before))
	h := admin.NewChannelHandler(service.NewChannelService(repository.NewChannelRepository(db), nil, nil, nil, nil), nil, nil)
	r := gin.New()
	r.GET("/upstream-operations/snapshot", h.UpstreamOperations)
	for _, path := range []string{"upstream"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/%s-operations/snapshot?account_id=%d&from=%s&until=%s", path, account, now.Add(-24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)), nil))
		require.Equal(t, 200, w.Code, w.Body.String())
		require.NotContains(t, w.Body.String(), "fixture-do-not-return")
		require.NotContains(t, w.Body.String(), `"credentials"`)
		require.Contains(t, w.Body.String(), `"token_read_rate":0.7`)
		require.Contains(t, w.Body.String(), `"upstream_group_status":"not_observed"`)
		require.Contains(t, w.Body.String(), `"source":"canonical_final_physical_http_attempt"`)
		var body struct {
			Data struct {
				Attribution string
				Usage       struct {
					Summary struct {
						Requests int
					}
					Rows []struct{ Key string }
				}
			}
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, 1, body.Data.Usage.Summary.Requests)
		require.Len(t, body.Data.Usage.Rows, 1)
		require.Equal(t, "https://http.example:8443/tenant/v1", body.Data.Usage.Rows[0].Key)
		require.Contains(t, body.Data.Attribution, "estimate")
	}
	// The same existing administrator endpoint exposes a WS generation's origin
	// explicitly, never an HTTP first-byte/SSE timing inferred from the handshake.
	_, err = db.Exec(`UPDATE request_timing_details SET detail=detail || jsonb_build_object(
 'ttft_mode','ws_turn','status',0,'total_ms',100,'terminal','completed','outcome','success',
 'raw_upstream',(detail->'raw_upstream') || jsonb_build_object('transport','ws_turn',
   'credential_source','websocket_handshake_credentials','attempt_number',1),
 'attempts',jsonb_build_array(jsonb_build_object('number',1,'kind','ws_turn','account_id',$1::bigint,
   'status',0,'start_ms',0,'end_ms',100,'events','{}'::jsonb,'route_fingerprint',repeat('b',64),
   'upstream_credential',jsonb_build_object('status','observed','source','websocket_handshake_credentials','fingerprint',repeat('a',64)))),
 'latency',jsonb_build_object('version',1,'ws_turn',jsonb_build_object('number',1,'source','physical_ws_turn_generation_start',
   'first_event',jsonb_build_object('status','measured','ms',0),
   'first_reasoning',jsonb_build_object('status','not_collected','ms',NULL),
   'first_text',jsonb_build_object('status','measured','ms',20))))
 WHERE usage_log_id IN (SELECT id FROM usage_logs WHERE account_id=$1)`, account)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/upstream-operations/snapshot?account_id=%d&from=%s&until=%s", account, now.Add(-24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)), nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"source":"canonical_physical_ws_turn_generation_start"`)
	require.Contains(t, w.Body.String(), `"transport":"ws_turn"`)
	var wsBody struct {
		Data struct {
			RawCache struct {
				Data struct {
					Rows []struct {
						Waits struct {
							FirstByte struct {
								Samples int
								MeanMS  *float64 `json:"mean_ms"`
							} `json:"first_byte"`
							FirstEvent struct {
								Samples int
								MeanMS  *float64 `json:"mean_ms"`
							} `json:"first_event"`
						}
					}
				}
			} `json:"raw_cache"`
		}
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &wsBody))
	require.Len(t, wsBody.Data.RawCache.Data.Rows, 1)
	require.Equal(t, 1, wsBody.Data.RawCache.Data.Rows[0].Waits.FirstEvent.Samples)
	require.NotNil(t, wsBody.Data.RawCache.Data.Rows[0].Waits.FirstEvent.MeanMS)
	require.Zero(t, *wsBody.Data.RawCache.Data.Rows[0].Waits.FirstEvent.MeanMS)
	require.Zero(t, wsBody.Data.RawCache.Data.Rows[0].Waits.FirstByte.Samples)
	require.Nil(t, wsBody.Data.RawCache.Data.Rows[0].Waits.FirstByte.MeanMS)
	require.NotContains(t, w.Body.String(), "fixture-do-not-return")
	var after string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT row_to_json(u)::text FROM usage_logs u WHERE account_id=$1`, account).Scan(&after))
	require.Equal(t, before, after, "the channel endpoint is read-only")
}

func TestUpstreamAttemptsHTTPTransportToPostgresToAdmin(t *testing.T) {
	for _, protocol := range []string{"responses", "messages", "gemini_native", "gemini_internal"} {
		t.Run(protocol, func(t *testing.T) {
			db := repository.ChannelOperationsTestDatabase()
			var account int64
			require.NoError(t, db.QueryRow(`INSERT INTO accounts(name,platform,type,credentials) VALUES('attempt transport fixture','openai','apikey','{}') RETURNING id`).Scan(&account))
			t.Cleanup(func() {
				_, _ = db.Exec(`DELETE FROM request_connection_traces WHERE detail->'attempts' @> jsonb_build_array(jsonb_build_object('account_id',$1::bigint))`, account)
				_, _ = db.Exec(`DELETE FROM accounts WHERE id=$1`, account)
			})
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					w.WriteHeader(502)
					_, _ = w.Write([]byte(`{"error":"fixture transient failure"}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if strings.HasPrefix(protocol, "gemini_") {
					write := func(chunk string) {
						if protocol == "gemini_internal" {
							chunk = `{"response":` + chunk + `}`
						}
						_, _ = fmt.Fprint(w, "data: "+chunk+"\n\n")
						w.(http.Flusher).Flush()
					}
					write(`{"candidates":[{"content":{"parts":[{"text":"fixture first text"}]}}]}`)
					time.Sleep(300 * time.Millisecond)
					write(`{"candidates":[{"content":{"parts":[{"text":"fixture second text"}]}}]}`)
					time.Sleep(10 * time.Millisecond)
					write(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":75,"candidatesTokenCount":100,"thoughtsTokenCount":20}}`)
					return
				}
				if protocol == "messages" {
					_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":75,\"cache_creation_input_tokens\":5}}}\n\n")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"fixture first text\"}}\n\n")
					w.(http.Flusher).Flush()
					time.Sleep(300 * time.Millisecond)
					_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"fixture second text\"}}\n\n")
					w.(http.Flusher).Flush()
					time.Sleep(10 * time.Millisecond)
					_, _ = fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":120,\"output_tokens_details\":{\"thinking_tokens\":20}}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture first text\"}\n\n")
				w.(http.Flusher).Flush()
				time.Sleep(300 * time.Millisecond)
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture second text\"}\n\n")
				w.(http.Flusher).Flush()
				time.Sleep(10 * time.Millisecond)
				_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"fixture complete text"}]}],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":75},"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}}}}`+"\n\n")
			}))
			defer upstream.Close()
			usage := repository.NewUsageLogRepository(nil, db)
			recorder, ok := usage.(middleware.ConnectionTraceRecorder)
			require.True(t, ok)
			client := repository.NewHTTPUpstream(nil)
			gateway := gin.New()
			gateway.Use(middleware.RequestLogger(), middleware.RequestTiming(recorder))
			ingressPath := "/v1/" + protocol
			if strings.HasPrefix(protocol, "gemini_") {
				ingressPath = "/v1beta/models/public-model:streamGenerateContent"
			}
			gateway.POST(ingressPath, func(c *gin.Context) {
				c.Set("ops_model", "public-model")
				c.Set("ops_stream", true)
				for _, model := range []string{"first-wire-model", "second-wire-model"} {
					path := "/v1/" + protocol
					payload := `{"model":"` + model + `","stream":true,"service_tier":"priority","reasoning":{"effort":"high"},"input":"fixture"}`
					if protocol == "responses" {
						verbosity := "low"
						if model == "second-wire-model" {
							verbosity = "high"
						}
						payload = `{"model":"` + model + `","stream":true,"service_tier":"priority","reasoning":{"effort":"high","summary":"auto"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"` + verbosity + `"},"input":"private-output-scope-prompt"}`
					}
					if protocol == "messages" {
						payload = `{"model":"` + model + `","stream":true,"service_tier":"standard_only","speed":"fast","thinking":{"type":"enabled","budget_tokens":4096},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"fixture"}]}`
					}
					if strings.HasPrefix(protocol, "gemini_") {
						path = "/v1beta/models/" + model + ":streamGenerateContent"
						payload = `{"contents":[{"parts":[{"text":"fixture"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":4096,"includeThoughts":true}}}`
						if protocol == "gemini_internal" {
							path, payload = "/v1internal:streamGenerateContent", `{"model":"`+model+`","project":"private-project","request":`+payload+`}`
						}
					}
					req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL+path, strings.NewReader(payload))
					if err != nil {
						c.AbortWithStatus(500)
						return
					}
					req.Header.Set("Authorization", "Bearer diagnostic-transport-fixture")
					req, _ = requesttiming.StartAttempt(req, account, 0)
					requesttiming.RecordAccountRoute(req.Context(), [32]byte{1})
					resp, err := client.Do(req, "", account, 1)
					if err != nil {
						c.AbortWithStatus(500)
						return
					}
					if resp.StatusCode >= 400 {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						continue
					}
					responseCtx := requesttiming.ResponseContext(c.Request.Context(), resp)
					scanner := bufio.NewScanner(resp.Body)
					for scanner.Scan() {
						if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
							if strings.HasPrefix(protocol, "gemini_") {
								requesttiming.GeminiContent(responseCtx, []byte(data), true)
							} else {
								requesttiming.Content(responseCtx, []byte(data))
							}
						}
					}
					_ = resp.Body.Close()
					if scanner.Err() != nil {
						c.AbortWithStatus(500)
						return
					}
					requesttiming.Output(responseCtx, false, false, "completed")
					requesttiming.Outcome(c.Request.Context(), "success", false)
					c.Data(200, "application/json", []byte(`{"ok":true}`))
					return
				}
				c.AbortWithStatus(502)
			})
			w := httptest.NewRecorder()
			gateway.ServeHTTP(w, httptest.NewRequest("POST", ingressPath, strings.NewReader(`{}`)))
			require.Equal(t, 200, w.Code, w.Body.String())
			require.EqualValues(t, 2, calls.Load())
			require.Eventually(t, func() bool {
				var count int
				err := db.QueryRow(`SELECT count(*) FROM request_connection_traces WHERE detail->'attempts' @> jsonb_build_array(jsonb_build_object('account_id',$1::bigint))`, account).Scan(&count)
				return err == nil && count == 1
			}, 3*time.Second, 10*time.Millisecond)
			h := admin.NewChannelHandler(service.NewChannelService(repository.NewChannelRepository(db), nil, nil, nil, nil), nil, nil)
			r := gin.New()
			r.GET("/snapshot", h.UpstreamOperations)
			result := httptest.NewRecorder()
			now := time.Now().UTC()
			r.ServeHTTP(result, httptest.NewRequest("GET", fmt.Sprintf("/snapshot?account_id=%d&from=%s&until=%s", account, now.Add(-time.Minute).Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339)), nil))
			require.Equal(t, 200, result.Code, result.Body.String())
			var body struct {
				Data struct {
					Attempts struct {
						Status string
						Data   struct {
							RawCache struct {
								Observed int      `json:"observed_attempts"`
								Unknown  int      `json:"unknown_attempts"`
								Rate     *float64 `json:"token_read_rate"`
							} `json:"raw_cache"`
							Summary struct {
								Attempts, Failed, Succeeded int
								LogicalRequests             int `json:"logical_requests"`
							}
							Rows []struct {
								ReasoningSummary string `json:"reasoning_summary"`
								EncryptedContent string `json:"encrypted_content"`
								TextVerbosity    string `json:"text_verbosity"`
								Output           struct {
									Gap  struct{ Samples int } `json:"max_gap"`
									Tail struct{ Samples int } `json:"tail_wait"`
									Rate struct {
										Samples int
										TPS     *float64 `json:"tokens_per_second"`
									}
								}
								Waits struct {
									Version   int
									Source    string
									FirstByte struct {
										Samples int
										MeanMS  *float64 `json:"mean_ms"`
									} `json:"first_byte"`
									FirstText struct {
										Samples int
										Unknown int `json:"unknown_samples"`
									} `json:"first_text"`
								}
							}
						}
					}
				}
			}
			require.NoError(t, json.Unmarshal(result.Body.Bytes(), &body))
			require.Equal(t, "ready", body.Data.Attempts.Status)
			require.Equal(t, 2, body.Data.Attempts.Data.Summary.Attempts)
			require.Equal(t, 1, body.Data.Attempts.Data.Summary.Failed)
			require.Equal(t, 1, body.Data.Attempts.Data.Summary.Succeeded)
			require.Equal(t, 1, body.Data.Attempts.Data.Summary.LogicalRequests)
			require.Contains(t, result.Body.String(), `"traffic_source":"gateway_ingress"`)
			require.Contains(t, result.Body.String(), `"source":"server_recorded_entrypoints"`)
			require.NotContains(t, result.Body.String(), "diagnostic-transport-fixture")
			for _, model := range []string{"first-wire-model", "second-wire-model"} {
				filtered := httptest.NewRecorder()
				r.ServeHTTP(filtered, httptest.NewRequest("GET", fmt.Sprintf("/snapshot?account_id=%d&model_scope=upstream&model=%s&from=%s&until=%s", account, model, now.Add(-time.Minute).Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339)), nil))
				require.Equal(t, 200, filtered.Code, filtered.Body.String())
				require.NoError(t, json.Unmarshal(filtered.Body.Bytes(), &body))
				require.Equal(t, "ready", body.Data.Attempts.Status)
				require.Equal(t, 1, body.Data.Attempts.Data.Summary.Attempts)
				if protocol == "responses" {
					require.Len(t, body.Data.Attempts.Data.Rows, 1)
					row := body.Data.Attempts.Data.Rows[0]
					require.Equal(t, "auto", row.ReasoningSummary)
					require.Equal(t, "requested", row.EncryptedContent)
					verbosity := "low"
					if model == "second-wire-model" {
						verbosity = "high"
					}
					require.Equal(t, verbosity, row.TextVerbosity)
					require.NotContains(t, filtered.Body.String(), "private-output-scope-prompt")
				}
				if model == "first-wire-model" {
					require.Equal(t, 1, body.Data.Attempts.Data.Summary.Failed)
					require.Zero(t, body.Data.Attempts.Data.Summary.Succeeded)
					require.Zero(t, body.Data.Attempts.Data.RawCache.Observed)
					require.Equal(t, 1, body.Data.Attempts.Data.RawCache.Unknown)
					require.Nil(t, body.Data.Attempts.Data.RawCache.Rate)
				} else {
					require.Equal(t, 1, body.Data.Attempts.Data.Summary.Succeeded)
					require.Zero(t, body.Data.Attempts.Data.Summary.Failed)
					require.Equal(t, 1, body.Data.Attempts.Data.RawCache.Observed)
					require.Zero(t, body.Data.Attempts.Data.RawCache.Unknown)
					require.InDelta(t, 0.75, *body.Data.Attempts.Data.RawCache.Rate, 0.000001)
				}
				require.Contains(t, filtered.Body.String(), `"upstream_model":"`+model+`"`)
				if strings.HasPrefix(protocol, "gemini_") {
					require.Contains(t, filtered.Body.String(), `"upstream_protocol":"gemini_stream_generate_content"`)
					require.Contains(t, filtered.Body.String(), `"thinking_mode":"budget_fixed"`)
					require.Contains(t, filtered.Body.String(), `"thinking_budget_tokens":4096`)
					require.Contains(t, filtered.Body.String(), `"include_thoughts":"enabled"`)
					require.Contains(t, filtered.Body.String(), `"reasoning_effort":"unspecified"`)
					require.NotContains(t, filtered.Body.String(), "private-project")
				} else if protocol == "messages" {
					require.Contains(t, filtered.Body.String(), `"service_tier":"standard_only"`)
					require.Contains(t, filtered.Body.String(), `"speed":"fast"`)
					require.Contains(t, filtered.Body.String(), `"thinking_mode":"enabled"`)
					require.Contains(t, filtered.Body.String(), `"thinking_budget_tokens":4096`)
				} else {
					require.Contains(t, filtered.Body.String(), `"service_tier":"priority"`)
				}
				if !strings.HasPrefix(protocol, "gemini_") {
					require.Contains(t, filtered.Body.String(), `"reasoning_effort":"high"`)
				}
				require.Contains(t, filtered.Body.String(), `"traffic_source":"gateway_ingress"`)
				require.Contains(t, filtered.Body.String(), `"unknown_attempts":0`)
				require.Len(t, body.Data.Attempts.Data.Rows, 1)
				waits := body.Data.Attempts.Data.Rows[0].Waits
				require.Equal(t, 1, waits.Version)
				require.Equal(t, "canonical_physical_http_attempt", waits.Source)
				require.Equal(t, 1, waits.FirstByte.Samples, "both the failed and recovered physical request retain their own byte wait")
				require.NotNil(t, waits.FirstByte.MeanMS)
				require.GreaterOrEqual(t, *waits.FirstByte.MeanMS, 0.0)
				output := body.Data.Attempts.Data.Rows[0].Output
				if model == "first-wire-model" {
					require.Zero(t, waits.FirstText.Samples)
					require.Equal(t, 1, waits.FirstText.Unknown)
					require.Zero(t, output.Gap.Samples)
					require.Zero(t, output.Rate.Samples)
					require.Nil(t, output.Rate.TPS)
				} else {
					require.Equal(t, 1, waits.FirstText.Samples)
					require.Zero(t, waits.FirstText.Unknown)
					require.Equal(t, 1, output.Gap.Samples)
					require.Equal(t, 1, output.Tail.Samples)
					require.Equal(t, 1, output.Rate.Samples)
					require.Greater(t, *output.Rate.TPS, 0.0)
				}
			}
			var billed int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_logs WHERE account_id=$1`, account).Scan(&billed))
			require.Zero(t, billed)
		})
	}
}
