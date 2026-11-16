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
	var after string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT row_to_json(u)::text FROM usage_logs u WHERE account_id=$1`, account).Scan(&after))
	require.Equal(t, before, after, "the channel endpoint is read-only")
}
