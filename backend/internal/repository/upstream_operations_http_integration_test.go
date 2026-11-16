//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
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
