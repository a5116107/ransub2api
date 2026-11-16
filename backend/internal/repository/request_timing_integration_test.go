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
