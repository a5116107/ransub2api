//go:build integration

package repository

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUpstreamAttemptsRetainRecoveredAndUnbilledFailures(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC().Truncate(time.Second)
	end := 100.0
	physical := func(number int, account int64, status int, terminal string) requesttiming.Attempt {
		return requesttiming.Attempt{Number: number, Kind: "http", AccountID: account, Status: status,
			Terminal: terminal, EndMS: &end, RouteFingerprint: strings.Repeat("b", 64),
			UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("a", 64)}}
	}
	write := func(s requesttiming.Snapshot) {
		s.Version, s.TraceID, s.StartedAt, s.TotalMS = 1, uuid.NewString(), now, 100
		t.Cleanup(func() {
			_, _ = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, s.TraceID)
		})
		require.NoError(t, repo.writeConnectionTrace(t.Context(), service.ConnectionTrace{TraceID: s.TraceID, StartedAt: now,
			RequestID: uuid.NewString(), UserID: f.user, APIKeyID: f.key, AccountID: f.outsider, GroupID: f.group,
			Source: "application", Model: "public-model", Path: "/v1/responses", Stream: true, Detail: s}))
	}
	write(requesttiming.Snapshot{Status: 200, Outcome: "success", Terminal: "completed", FinalResponseAttempt: 2,
		Attempts: []requesttiming.Attempt{physical(1, f.account, 502, "failed"), physical(2, f.outsider, 200, "completed")}})
	write(requesttiming.Snapshot{Status: 502, Outcome: "failed", Attempts: []requesttiming.Attempt{physical(1, f.account, 503, "")}})
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Minute), Until: now.Add(time.Minute), GroupID: f.group,
		AccountID: f.account, Model: "public-model", ModelScope: "public", Dimension: "account", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	var body struct {
		Attempts struct {
			Status string
			Data   struct {
				Summary struct {
					Attempts        int
					Failed          int
					Succeeded       int
					LogicalRequests int `json:"logical_requests"`
				}
			}
		}
	}
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, "ready", body.Attempts.Status, string(raw))
	require.Equal(t, 2, body.Attempts.Data.Summary.Attempts)
	require.Equal(t, 2, body.Attempts.Data.Summary.Failed, "a later success must not erase the first upstream's failure")
	require.Zero(t, body.Attempts.Data.Summary.Succeeded)
	require.Equal(t, 2, body.Attempts.Data.Summary.LogicalRequests)
	var usage int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_logs WHERE api_key_id=$1`, f.key).Scan(&usage))
	require.Zero(t, usage, "unbilled failures must remain observable without creating billing rows")
	q.AccountID = 0
	out, err = f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	raw, err = json.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, 3, body.Attempts.Data.Summary.Attempts)
	require.Equal(t, 2, body.Attempts.Data.Summary.Failed)
	require.Equal(t, 1, body.Attempts.Data.Summary.Succeeded)
	require.Equal(t, 2, body.Attempts.Data.Summary.LogicalRequests)
}

func TestUpstreamAttemptsDeduplicateFallbackAndKeepUnknownCapture(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC().Truncate(time.Second)
	end := 100.0
	s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), StartedAt: now, TotalMS: 100, Status: 200,
		Attempts: []requesttiming.Attempt{{Kind: "http", Number: 1, AccountID: f.account, EndMS: &end, Status: 200, Terminal: "completed",
			RouteFingerprint: strings.Repeat("b", 64), UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("a", 64)}}}}
	row := service.ConnectionTrace{Source: "application", TraceID: s.TraceID, StartedAt: now, RequestID: uuid.NewString(),
		APIKeyID: f.key, UserID: f.user, AccountID: f.account, GroupID: f.group, Model: "public-model", Path: "/v1/responses", Detail: s}
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM request_timing_details WHERE trace_id=$1`, s.TraceID)
		_, _ = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE api_key_id=$1`, f.key)
	})
	require.NoError(t, repo.writeConnectionTrace(t.Context(), row))
	require.NoError(t, repo.writeConnectionTrace(t.Context(), row))
	log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account, GroupID: &f.group, Model: "billing-model", RequestedModel: "public-model", RequestID: row.RequestID, CreatedAt: now}
	_, err := repo.Create(t.Context(), log)
	require.NoError(t, err)
	require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: row.RequestID, apiKeyID: f.key, data: s}))
	// The fallback window uses persistence time. Align this fixture's two
	// timestamps so the deduplication oracle compares the same sample time.
	_, err = integrationDB.Exec(`UPDATE request_timing_details SET created_at=$2 WHERE trace_id=$1`, s.TraceID, now)
	require.NoError(t, err)
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Minute), Until: now.Add(time.Minute), AccountID: f.account, GroupID: f.group, Model: "public-model", ModelScope: "public", Dimension: "account", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	read := func() map[string]any {
		t.Helper()
		out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
		require.NoError(t, err)
		require.Equal(t, "ready", out.Attempts.Status)
		var data map[string]any
		require.NoError(t, json.Unmarshal(out.Attempts.Data, &data))
		return data
	}
	first := read()
	summary, ok := first["summary"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 1.0, summary["attempts"])
	_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, s.TraceID)
	require.NoError(t, err)
	fallback := read()
	require.Equal(t, first["summary"], fallback["summary"])
	// Missing versions/capture never turn into zero failures with full coverage.
	for _, patch := range []string{`{"physical_upstream":null}`, `{"physical_upstream":{"version":9,"complete":true,"attempts":[]}}`,
		`{"physical_upstream":{"version":1,"complete":true,"attempts":{}}}`, `{"physical_upstream":{"version":1,"complete":false,"attempts":[]}}`} {
		_, err = integrationDB.Exec(`UPDATE request_timing_details SET detail=detail || $2::jsonb WHERE trace_id=$1`, s.TraceID, patch)
		require.NoError(t, err)
		unknown := read()
		capture, ok := unknown["capture"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, 1.0, capture["unknown_traces"])
		require.Equal(t, 0.0, capture["complete_traces"])
		metrics, ok := unknown["summary"].(map[string]any)
		require.True(t, ok)
		require.Nil(t, metrics["confirmed_success_rate"])
	}
	// The physical model is not the final billing alias.
	q.ModelScope = "upstream"
	out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, "unsupported_scope", out.Attempts.Status)
	q.ModelScope = "public"
	q.Model = "does-not-exist"
	empty := read()
	metrics, ok := empty["summary"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 0.0, metrics["attempts"])
}
