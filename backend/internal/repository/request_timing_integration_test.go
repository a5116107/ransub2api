//go:build integration

package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRequestTimingsPostgresPhysicalWaitProjectionIsReadOnly(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account,
		Model: "fixture", RequestID: uuid.NewString()}
	_, err := repo.Create(t.Context(), log)
	require.NoError(t, err)
	end := 500.0
	s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), TotalMS: 600,
		Status: 200, Attempts: []requesttiming.Attempt{{Number: 1, Kind: "http", AccountID: f.account,
			StartMS: 100, EndMS: &end, Status: 200, Events: map[string]float64{
				"headers_written": 120, "request_written": 150, "first_byte": 200,
				"first_sse_event_flush": 250, "first_reasoning": 300, "first_visible_text": 400}}}}
	require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: f.key, data: s}))
	for _, state := range []string{"current", "legacy", "future"} {
		t.Run(state, func(t *testing.T) {
			if state == "legacy" {
				_, err = integrationDB.ExecContext(t.Context(), `UPDATE request_timing_details
 SET detail=(detail-'latency') || '{"extension":{"retained":true}}'::jsonb WHERE usage_log_id=$1`, log.ID)
				require.NoError(t, err)
			}
			if state == "future" {
				_, err = integrationDB.ExecContext(t.Context(), `UPDATE request_timing_details
 SET detail=jsonb_set(detail,'{version}','9'::jsonb) WHERE usage_log_id=$1`, log.ID)
				require.NoError(t, err)
			}
			var before, after string
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail::text FROM request_timing_details WHERE usage_log_id=$1`, log.ID).Scan(&before))
			rows, err := repo.RequestTimings(t.Context(), log.ID)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			if state == "future" {
				require.JSONEq(t, before, string(rows[0]), "unknown versions cannot be interpreted as the current contract")
			} else {
				var projected requesttiming.Snapshot
				require.NoError(t, json.Unmarshal(rows[0], &projected))
				require.NotNil(t, projected.Latency)
				require.Len(t, projected.Latency.Attempts, 1)
				waits := projected.Latency.Attempts[0]
				for i, value := range []requesttiming.Measurement{waits.FirstByteWait, waits.FirstEventWait, waits.FirstReasoningWait, waits.FirstTextWait} {
					require.Equal(t, "measured", value.Status)
					require.NotNil(t, value.MS)
					require.Equal(t, []float64{50, 100, 150, 250}[i], *value.MS)
				}
				if state == "legacy" {
					require.Contains(t, string(rows[0]), `"extension":{"retained":true}`)
				}
			}
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail::text FROM request_timing_details WHERE usage_log_id=$1`, log.ID).Scan(&after))
			require.Equal(t, before, after, "reading new fields must leave retained diagnostic evidence unchanged")
		})
	}
}

func TestRequestTimingsPostgresWSTurnWaitProjectionIsReadOnly(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	for _, state := range []string{"current", "legacy_latency", "legacy_events", "future_latency", "future_trace"} {
		t.Run(state, func(t *testing.T) {
			log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account,
				Model: "ws-fixture", RequestID: uuid.NewString()}
			_, err := repo.Create(t.Context(), log)
			require.NoError(t, err)
			end := 50.0
			s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), TotalMS: 60, TTFTMode: "ws_turn",
				Attempts: []requesttiming.Attempt{{Number: 1, Kind: "ws_turn", AccountID: f.account, EndMS: &end,
					Events: map[string]float64{"first_upstream_event": 0, "first_reasoning": 20, "first_visible_text": 30,
						"first_byte": 1, "first_sse_event_flush": 2}}}}
			require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: f.key, data: s}))
			mutation := "detail"
			switch state {
			case "legacy_latency":
				mutation = "detail-'latency'"
			case "legacy_events":
				mutation = "(detail-'latency') #- '{attempts,0,events,first_upstream_event}'"
			case "future_latency":
				mutation = "jsonb_set(detail,'{latency,version}','9')"
			case "future_trace":
				mutation = "jsonb_set(detail,'{version}','9')"
			}
			_, err = integrationDB.ExecContext(t.Context(), `UPDATE request_timing_details SET detail=(`+mutation+`) || '{"extension":{"retained":true}}'::jsonb WHERE usage_log_id=$1`, log.ID)
			require.NoError(t, err)
			var before, after string
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail::text FROM request_timing_details WHERE usage_log_id=$1`, log.ID).Scan(&before))
			rows, err := repo.RequestTimings(t.Context(), log.ID)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			if state == "future_latency" || state == "future_trace" {
				require.JSONEq(t, before, string(rows[0]))
			} else {
				var projected requesttiming.Snapshot
				require.NoError(t, json.Unmarshal(rows[0], &projected))
				require.NotNil(t, projected.Latency)
				require.Empty(t, projected.Latency.Attempts)
				require.NotNil(t, projected.Latency.WSTurn)
				waits := projected.Latency.WSTurn
				require.Equal(t, "physical_ws_turn_generation_start", waits.Source)
				if state == "legacy_events" {
					require.Equal(t, "not_collected", waits.FirstEvent.Status)
					require.Nil(t, waits.FirstEvent.MS, "never borrow a handshake byte or downstream SSE event")
				} else {
					require.Equal(t, "measured", waits.FirstEvent.Status)
					require.Zero(t, *waits.FirstEvent.MS)
				}
				require.Equal(t, 20.0, *waits.FirstReasoning.MS)
				require.Equal(t, 30.0, *waits.FirstText.MS)
				require.Contains(t, string(rows[0]), `"extension":{"retained":true}`)
			}
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail::text FROM request_timing_details WHERE usage_log_id=$1`, log.ID).Scan(&after))
			require.Equal(t, before, after)
		})
	}
}
